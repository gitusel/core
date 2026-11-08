package email

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"windshift/internal/utils"
)

// GraphClient talks to the Microsoft Graph v1.0 send endpoints using the
// client-credentials application flow. The zero-config constructor wires the
// shared SSRF-safe outbound client so Graph calls honor the process-wide
// local-connection policy; tests inject a fake endpoint URL and transport.
type GraphClient struct {
	apiBase   string // e.g. https://graph.microsoft.com
	tokenBase string // e.g. https://login.microsoftonline.com — independent host
	http      *http.Client
}

// Defaults per Microsoft identity platform: tokens come from
// login.microsoftonline.com while the API lives on graph.microsoft.com. The
// API root must carry the version segment: sendMail resolves
// https://graph.microsoft.com/v1.0/users/{upn}/sendMail — a bare
// graph.microsoft.com/users/... yields 404 Invalid version.
const (
	defaultGraphAPIBase   = "https://graph.microsoft.com/v1.0"
	defaultGraphTokenBase = "https://login.microsoftonline.com"
)

// NewGraphClient builds a client against the given Graph root (empty string
// defaults to the commercial endpoint). tokenBase, when non-empty, overrides
// the login host (tests); empty keeps the default. A nil httpClient selects
// the shared SSRF-safe outbound client (utils.NewHTTPClient), matching repo
// policy for all server-side egress.
func NewGraphClient(apiBase, tokenBase string, httpClient *http.Client) *GraphClient {
	if apiBase == "" {
		apiBase = defaultGraphAPIBase
	}
	if tokenBase == "" {
		tokenBase = defaultGraphTokenBase
	}
	if httpClient == nil {
		httpClient = utils.NewHTTPClient(30 * time.Second)
	}
	return &GraphClient{
		apiBase:   strings.TrimRight(apiBase, "/"),
		tokenBase: strings.TrimRight(tokenBase, "/"),
		http:      httpClient,
	}
}

// resolveTokenURL builds the tenant token endpoint:
// {tokenBase}/{tenant}/oauth2/v2.0/token.
func (g *GraphClient) resolveTokenURL(tenantID string) string {
	return fmt.Sprintf("%s/%s/oauth2/v2.0/token", g.tokenBase, url.PathEscape(tenantID))
}

// TokenURL exposes the resolved token endpoint for tests and diagnostics.
func (g *GraphClient) TokenURL(tenantID string) string { return g.resolveTokenURL(tenantID) }

// FetchToken mints an application access token via OAuth2 client
// credentials against the tenant's v2.0 token endpoint with the
// https://graph.microsoft.com/.default scope, which resolves to the app
// registration's granted application permissions (Mail.Send for sending).
func (g *GraphClient) FetchToken(ctx context.Context, tenantID, clientID, clientSecret string) (*OAuthTokens, error) {
	params := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"scope":         {"https://graph.microsoft.com/.default"},
	}

	httpClient := g.http
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.resolveTokenURL(tenantID), strings.NewReader(params.Encode()))
	if err != nil {
		return nil, fmt.Errorf("graph token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graph token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("graph token response read: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &errResp)
		detail := errResp.Error
		if errResp.ErrorDescription != "" {
			detail = fmt.Sprintf("%s: %s", errResp.Error, errResp.ErrorDescription)
		}
		if detail == "" {
			detail = strings.TrimSpace(string(body))
		}
		return nil, fmt.Errorf("graph token request failed (%d): %s", resp.StatusCode, detail)
	}

	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("graph token response parse: %w", err)
	}
	if token.AccessToken == "" {
		return nil, fmt.Errorf("graph token response missing access_token")
	}

	expiresAt := time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	return &OAuthTokens{
		AccessToken: token.AccessToken,
		TokenType:   token.TokenType,
		ExpiresAt:   &expiresAt,
	}, nil
}

// graphError mirrors the Graph error envelope for readable failures.
type graphError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// SendMime posts a fully built MIME message via Graph sendMail using the
// MIME format contract: Content-Type text/plain, base64-encoded raw body.
// A 202 is the only success; anything else is mapped to a descriptive error.
func (g *GraphClient) SendMime(ctx context.Context, accessToken, mailboxUPN string, mime []byte) error {
	endpoint := fmt.Sprintf("%s/users/%s/sendMail", g.apiBase, url.PathEscape(mailboxUPN))

	httpClient := g.http
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(base64.StdEncoding.EncodeToString(mime)))
	if err != nil {
		return fmt.Errorf("graph sendMail request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("graph sendMail request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusAccepted {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("graph sendMail response read: %w", err)
	}
	var envelope graphError
	_ = json.Unmarshal(body, &envelope)

	detail := envelope.Error.Message
	if envelope.Error.Code != "" {
		detail = fmt.Sprintf("%s: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	switch resp.StatusCode {
	case http.StatusBadRequest:
		return fmt.Errorf("graph sendMail rejected the message (400): %s", detail)
	case http.StatusUnauthorized:
		return fmt.Errorf("graph sendMail unauthorized (401): token expired or invalid; %s", detail)
	case http.StatusForbidden:
		return fmt.Errorf("graph sendMail forbidden (403): application access policy likely excludes this mailbox; %s", detail)
	case http.StatusNotFound:
		return fmt.Errorf("graph sendMail mailbox not found (404): %s", detail)
	case http.StatusTooManyRequests:
		return fmt.Errorf("graph sendMail throttled (429): %s", detail)
	default:
		return fmt.Errorf("graph sendMail failed (%d): %s", resp.StatusCode, detail)
	}
}
