// Package graph implements a minimal Microsoft Graph client. It is deliberately
// generic: the same token acquisition, request pipeline and error handling back
// every Microsoft Graph workload (Mail today; Teams and SharePoint can be added
// as sibling files that use Client.Do). Nothing in this package references SMTP
// or any Windshift mail-specific code; it is a thin, reusable API client.
package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"windshift/internal/utils"
)

// DefaultBaseURL is the Microsoft Graph v1.0 root. Override for sovereign
// clouds (e.g. GCC High / DoD) or for tests.
const DefaultBaseURL = "https://graph.microsoft.com/v1.0"

// DefaultTokenURL is the commercial cloud v2.0 endpoint. Overridden in tests
// and for sovereign clouds by ClientConfig.TokenURL.
const DefaultTokenURL = "https://login.microsoftonline.com"

// Scope is the default scope requested by Windshift: it only ever calls
// /sendMail, which requires Mail.Send. Do not widen this without a
// corresponding feature.
const Scope = "https://graph.microsoft.com/.default"

// maxResponseBytes caps how much of a Graph response body we read, so a
// hostile or malfunctioning endpoint cannot exhaust memory.
const maxResponseBytes = 4 << 20

// Config configures a Client. All fields except HTTPClient are required.
type Config struct {
	TenantID     string
	ClientID     string
	ClientSecret string
	// BaseURL defaults to DefaultBaseURL.
	BaseURL string
	// TokenURL defaults to DefaultTokenURL (commercial cloud).
	TokenURL string
	// Scope defaults to Scope.
	Scope string
	// HTTPClient, if set, is used for both token acquisition and API calls.
	// Defaults to an SSRF-safe client.
	HTTPClient *http.Client
}

// Client is a Microsoft Graph client. It is safe for concurrent use.
type Client struct {
	cfg        Config
	httpClient *http.Client
	baseURL    string
	tokenURL   string
	scope      string

	mu        sync.Mutex
	cached    string
	expiresAt time.Time
}

// NewClient validates cfg and returns a ready-to-use client.
func NewClient(cfg Config) (*Client, error) {
	cfg.TenantID = strings.TrimSpace(cfg.TenantID)
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	cfg.ClientSecret = strings.TrimSpace(cfg.ClientSecret)
	if err := validateTenantID(cfg.TenantID); err != nil {
		return nil, err
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("graph: client ID is required")
	}
	if cfg.ClientSecret == "" {
		return nil, fmt.Errorf("graph: client secret is required")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	if err := validateEndpoint(base); err != nil {
		return nil, fmt.Errorf("graph: base URL: %w", err)
	}
	tokenURL := strings.TrimRight(strings.TrimSpace(cfg.TokenURL), "/")
	switch {
	case tokenURL == "":
		tokenURL = DefaultTokenURL + "/" + url.PathEscape(cfg.TenantID) + "/oauth2/v2.0/token"
	case !strings.Contains(tokenURL, "oauth2") && !strings.HasSuffix(tokenURL, "token"):
		tokenURL = tokenURL + "/" + url.PathEscape(cfg.TenantID) + "/oauth2/v2.0/token"
	}
	if err := validateEndpoint(tokenURL); err != nil {
		return nil, fmt.Errorf("graph: token URL: %w", err)
	}
	scope := strings.TrimSpace(cfg.Scope)
	if scope == "" {
		scope = Scope
	}
	return &Client{
		cfg:        cfg,
		httpClient: refuseRedirects(cfg.HTTPClient),
		baseURL:    base,
		tokenURL:   tokenURL,
		scope:      scope,
	}, nil
}

// tokenResponse mirrors the v2.0 token endpoint payload.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// token returns a valid access token, refreshing it when missing or within a
// minute of expiry. Client-credentials tokens are cached by credential so a
// per-send client does not call the token endpoint again. The cache lock is
// not held during the HTTP call. A cancelled caller does not store a token
// and does not fail a shared fetch for other callers.
func (c *Client) token(ctx context.Context) (string, error) {
	if c.cfg.ClientSecret == "" {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.cached != "" && time.Now().Before(c.expiresAt.Add(-time.Minute)) {
			return c.cached, nil
		}
		return "", fmt.Errorf("graph: bearer token is missing or expired")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	key := tokenKey{tokenURL: c.tokenURL, clientID: c.cfg.ClientID, secret: c.cfg.ClientSecret, scope: c.scope}
	tokenMu.Lock()
	if entry, ok := tokenEntries[key]; ok {
		if time.Now().Before(entry.expiresAt.Add(-time.Minute)) {
			tokenMu.Unlock()
			return entry.accessToken, nil
		}
		delete(tokenEntries, key)
	}
	if call, ok := tokenCalls[key]; ok {
		tokenMu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-call.done:
			if call.err != nil && ctx.Err() == nil && errors.Is(call.err, context.Canceled) {
				return c.token(ctx)
			}
			return call.tok, call.err
		}
	}
	call := &tokenCall{done: make(chan struct{})}
	tokenCalls[key] = call
	tokenMu.Unlock()

	tok, expiresAt, err := c.fetchToken(ctx)

	tokenMu.Lock()
	if tokenCalls[key] == call {
		delete(tokenCalls, key)
	}
	if err == nil {
		tokenEntries[key] = tokenEntry{accessToken: tok, expiresAt: expiresAt}
	}
	call.tok, call.err = tok, err
	close(call.done)
	tokenMu.Unlock()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	return tok, err
}

func (c *Client) fetchToken(ctx context.Context) (string, time.Time, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"scope":         {c.scope},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("graph: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("graph: token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("graph: read token response: %w", err)
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", time.Time{}, fmt.Errorf("graph: decode token response (status %d): %w", resp.StatusCode, err)
	}
	if tok.AccessToken == "" {
		if tok.Error != "" {
			return "", time.Time{}, fmt.Errorf("graph: token endpoint returned %q: %s", tok.Error, tok.ErrorDesc)
		}
		return "", time.Time{}, fmt.Errorf("graph: token endpoint returned empty token (status %d)", resp.StatusCode)
	}
	expiresIn := time.Duration(tok.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = time.Hour
	}
	return tok.AccessToken, time.Now().Add(expiresIn), nil
}

// AccessToken returns a valid bearer token for callers that must attach it to
// a mailbox-scoped Graph request.
func (c *Client) AccessToken(ctx context.Context) (string, error) {
	return c.token(ctx)
}

// Do issues an authenticated JSON request against the Graph API. `path` is
// relative to the configured base URL (e.g. "/users/{id}/sendMail"). A nil
// body sends no payload. On a non-2xx status it returns an error containing
// the status and a bounded excerpt of the response.
func (c *Client) Do(ctx context.Context, method, path string, body any) error {
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("graph: encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("graph: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("graph: %s %s failed: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("graph: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		excerpt := strings.TrimSpace(string(respBody))
		if len(excerpt) > 300 {
			excerpt = excerpt[:300] + "…"
		}
		return fmt.Errorf("graph: %s %s: HTTP %d: %s", method, path, resp.StatusCode, excerpt)
	}
	return nil
}

// PostRaw issues an authenticated request whose body is sent unchanged.
// sendMail uses it for the text/plain base64 MIME form, which is the only
// Graph send that preserves Message-ID, In-Reply-To, and References.
func (c *Client) PostRaw(ctx context.Context, path, contentType string, body []byte) error {
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("graph: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("graph: POST %s failed: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("graph: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		excerpt := strings.TrimSpace(string(respBody))
		if len(excerpt) > 300 {
			excerpt = excerpt[:300] + "…"
		}
		return fmt.Errorf("graph: POST %s: HTTP %d: %s", path, resp.StatusCode, excerpt)
	}
	return nil
}

// NewBearerClient builds a client that sends a delegated access token.
// It does not perform client-credentials token acquisition.
func NewBearerClient(baseURL, token string, hc *http.Client) (*Client, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("graph: bearer token is required")
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	if hc == nil {
		hc = utils.NewHTTPClient(30 * time.Second)
	}
	if err := validateEndpoint(base); err != nil {
		return nil, fmt.Errorf("graph: base URL: %w", err)
	}
	hc = refuseRedirects(hc)
	return &Client{
		httpClient: hc,
		baseURL:    base,
		cached:     token,
		expiresAt:  time.Now().Add(24 * time.Hour),
	}, nil
}

// Get performs an authenticated GET. rawURL may be absolute (a delta nextLink)
// or a path relative to the client base URL.
func (c *Client) Get(ctx context.Context, rawURL, accept string) ([]byte, error) {
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := rawURL
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		if !strings.HasPrefix(rawURL, "/") {
			rawURL = "/" + rawURL
		}
		endpoint = c.baseURL + rawURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("graph: build GET: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graph: GET %s failed: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("graph: read GET: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		excerpt := strings.TrimSpace(string(body))
		if len(excerpt) > 300 {
			excerpt = excerpt[:300] + "…"
		}
		return nil, fmt.Errorf("graph: GET %s: HTTP %d: %s", endpoint, resp.StatusCode, excerpt)
	}
	return body, nil
}

// CheckCredentials acquires an app-only token. Channel tests use it to
// validate tenant, client id, and secret without sending mail.
func CheckCredentials(ctx context.Context, cfg Config) error {
	c, err := NewClient(cfg)
	if err != nil {
		return err
	}
	_, err = c.token(ctx)
	return err
}

type tokenKey struct {
	tokenURL string
	clientID string
	secret   string
	scope    string
}

type tokenEntry struct {
	accessToken string
	expiresAt   time.Time
}

type tokenCall struct {
	done chan struct{}
	tok  string
	err  error
}

var (
	tokenMu      sync.Mutex
	tokenEntries = map[tokenKey]tokenEntry{}
	tokenCalls   = map[tokenKey]*tokenCall{}
)

func validateTenantID(id string) error {
	if id == "" || strings.ContainsAny(id, "/?#\\ \t") || strings.Contains(id, "..") {
		return fmt.Errorf("graph: invalid tenant ID")
	}
	return nil
}

func validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("graph: invalid endpoint")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
	}
	return fmt.Errorf("graph: endpoint must be https")
}

// refuseRedirects stops the OAuth client from following a 307/308 that would
// forward the client secret or bearer token to another host.
func refuseRedirects(base *http.Client) *http.Client {
	if base == nil {
		base = utils.NewHTTPClient(30 * time.Second)
	}
	clone := *base
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		loc := ""
		if req != nil && req.URL != nil {
			loc = req.URL.Redacted()
		}
		return fmt.Errorf("graph: refused redirect to %s", loc)
	}
	return &clone
}
