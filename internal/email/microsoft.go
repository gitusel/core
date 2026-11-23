package email

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"windshift/internal/graph"
	"windshift/internal/mailbox"
	"windshift/internal/models"
)

// Microsoft 365 IMAP settings
const (
	MicrosoftIMAPHost = "outlook.office365.com"
	MicrosoftIMAPPort = 993
)

// Microsoft OAuth endpoints
const (
	microsoftAuthURLTemplate = "https://login.microsoftonline.com/%s/oauth2/v2.0/authorize"
)

// microsoftTokenURLTemplate is the v2.0 token endpoint. Tests replace it with
// an in-process server; production uses the commercial cloud URL.
var microsoftTokenURLTemplate = "https://login.microsoftonline.com/%s/oauth2/v2.0/token" //nolint:gosec // G101 false positive: OAuth endpoint URL, not a credential

// MicrosoftDefaultScopes is the delegated Graph consent for reading and
// updating the configured mailbox. No identity scopes and no IMAP or SMTP
// scope. A grant issued for Outlook IMAP cannot call Graph, and refresh
// cannot add consent the user never gave — that mailbox must be reauthorized.
// NewMicrosoftProvider rewrites a persisted IMAP-only scope list to this set
// so reconnect and token refresh request Graph instead of polling with an
// unusable Outlook token.
var MicrosoftDefaultScopes = []string{
	"https://graph.microsoft.com/Mail.ReadWrite",
	"offline_access",
}

// MicrosoftProvider implements OAuth email provider for Microsoft 365
type MicrosoftProvider struct {
	ClientID     string
	ClientSecret string
	TenantID     string // "common" for multi-tenant, or specific tenant ID
	Scopes       []string

	// graphBaseURL and httpClient redirect Graph calls in tests.
	graphBaseURL string
	httpClient   *http.Client
}

// NewMicrosoftProvider creates a new Microsoft 365 email provider.
// Empty or Outlook-IMAP scope lists become MicrosoftDefaultScopes. An explicit
// Graph Mail.Read or Mail.ReadWrite list is kept, with offline_access added
// when it is missing so refresh can still run.
func NewMicrosoftProvider(clientID, clientSecret, tenantID string, scopes []string) *MicrosoftProvider {
	if tenantID == "" {
		tenantID = "common"
	}
	return &MicrosoftProvider{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TenantID:     tenantID,
		Scopes:       microsoftGraphScopes(scopes),
	}
}

// GetType returns the provider type identifier
func (p *MicrosoftProvider) GetType() string {
	return models.EmailProviderTypeMicrosoft
}

// GetIMAPServer returns Microsoft 365 IMAP server details
func (p *MicrosoftProvider) GetIMAPServer(config *models.ChannelConfig) (host string, port int) {
	return MicrosoftIMAPHost, MicrosoftIMAPPort
}

// GetOAuthURL returns the Microsoft authorization URL. prompt=select_account
// forces the account picker: without it Entra silently reuses the browser's
// existing session, so an admin already signed in as themselves cannot
// authorize a different mailbox without logging out or using a private window.
func (p *MicrosoftProvider) GetOAuthURL(state, redirectURI string) string {
	authURL := fmt.Sprintf(microsoftAuthURLTemplate, p.TenantID)
	params := url.Values{
		"client_id":     {p.ClientID},
		"response_type": {"code"},
		"redirect_uri":  {redirectURI},
		"response_mode": {"query"},
		"scope":         {strings.Join(p.Scopes, " ")},
		"state":         {state},
		"prompt":        {"select_account"},
	}
	return authURL + "?" + params.Encode()
}

// ExchangeCode exchanges an authorization code for tokens
func (p *MicrosoftProvider) ExchangeCode(ctx context.Context, code, redirectURI string) (*OAuthTokens, error) {
	tokenURL := fmt.Sprintf(microsoftTokenURLTemplate, p.TenantID)

	params := url.Values{
		"client_id":     {p.ClientID},
		"client_secret": {p.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
		"scope":         {strings.Join(p.Scopes, " ")},
	}

	return exchangeOAuthToken(ctx, tokenURL, params)
}

// RefreshToken refreshes an expired access token. A refreshed token that is
// still an Outlook IMAP grant is rejected so it is not stored and then used
// for a Graph poll that can never succeed.
func (p *MicrosoftProvider) RefreshToken(ctx context.Context, refreshToken string) (*OAuthTokens, error) {
	tokenURL := fmt.Sprintf(microsoftTokenURLTemplate, p.TenantID)

	params := url.Values{
		"client_id":     {p.ClientID},
		"client_secret": {p.ClientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
		"scope":         {strings.Join(p.Scopes, " ")},
	}

	tokens, err := exchangeOAuthToken(ctx, tokenURL, params)
	if err != nil {
		return nil, err
	}

	// Microsoft may return a new refresh token
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = refreshToken
	}
	if err := microsoftGrantRequiresReauthorization(tokens.AccessToken); err != nil {
		return nil, err
	}

	return tokens, nil
}

// OpenMailbox connects the configured mailbox through Microsoft Graph and
// returns it as a transport-neutral Mailbox. No IMAP client is involved: Graph
// mail is resumed from a delta link, not from a UID watermark.
//
// An Outlook-audience access token is refused before any Graph call, so a
// legacy IMAP grant cannot look like a configured mailbox that never receives
// mail. Reauthorize the mailbox; refresh cannot migrate that grant.
func (p *MicrosoftProvider) OpenMailbox(ctx context.Context, config *models.ChannelConfig) (mailbox.Mailbox, error) {
	box, err := p.openGraphMailbox(ctx, config)
	if err != nil {
		return nil, err
	}
	return graph.NewGraphMailbox(box), nil
}

// Connect refuses to present Graph mail as an IMAP client.
//
// The Provider interface keeps Connect because IMAP-backed providers need it,
// but Graph has no UID space to select or fetch by. Callers must use
// OpenMailbox; returning an adapter that fabricates SelectData and per-poll
// UIDs is what this replaces.
func (p *MicrosoftProvider) Connect(context.Context, *models.ChannelConfig) (IMAPClient, error) {
	return nil, fmt.Errorf("microsoft Graph mailboxes are opened through MailboxProvider.OpenMailbox, not an IMAP client")
}

// openGraphMailbox validates the delegated grant and builds the Graph reader.
func (p *MicrosoftProvider) openGraphMailbox(ctx context.Context, config *models.ChannelConfig) (*graph.GraphMailbox, error) {
	mailbox := strings.TrimSpace(config.EmailOAuthEmail)
	if mailbox == "" {
		mailbox = strings.TrimSpace(config.EmailOAuthMailbox)
	}
	if config.EmailOAuthAccessToken == "" {
		return nil, fmt.Errorf("no OAuth access token configured")
	}
	if mailbox == "" {
		return nil, fmt.Errorf("no OAuth email address configured")
	}
	if err := microsoftGrantRequiresReauthorization(config.EmailOAuthAccessToken); err != nil {
		return nil, err
	}
	return graph.OpenGraphMailbox(ctx, graph.GraphMailboxConfig{
		BaseURL:    p.graphBaseURL,
		Token:      config.EmailOAuthAccessToken,
		Mailbox:    mailbox,
		HTTPClient: p.httpClient,
	})
}

// TestConnection checks the delegated Graph token and that the configured
// folder exists. It does not dial IMAP. A missing folder is an error, not an
// empty mailbox.
func (p *MicrosoftProvider) TestConnection(ctx context.Context, config *models.ChannelConfig) error {
	box, err := p.openGraphMailbox(ctx, config)
	if err != nil {
		return err
	}
	folder := strings.TrimSpace(config.EmailMailbox)
	if folder == "" {
		folder = "INBOX"
	}
	// SelectFolder only maps well-known names locally; validating connection
	// requires confirming the folder exists in the mailbox, which bindFolder
	// does with a real Graph call and a not-found error.
	return box.ValidateFolder(ctx, folder)
}

func microsoftGraphScopes(scopes []string) []string {
	kept := make([]string, 0, len(scopes)+1)
	hasMail := false
	hasOffline := false
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" || droppedMicrosoftScope(scope) {
			continue
		}
		if scope == "offline_access" {
			hasOffline = true
			kept = append(kept, scope)
			continue
		}
		if graphMailContentScope(scope) {
			hasMail = true
		}
		kept = append(kept, scope)
	}
	if !hasMail {
		return append([]string(nil), MicrosoftDefaultScopes...)
	}
	if !hasOffline {
		kept = append(kept, "offline_access")
	}
	return kept
}

func droppedMicrosoftScope(scope string) bool {
	s := strings.ToLower(scope)
	switch s {
	case "openid", "profile", "email":
		return true
	}
	return strings.Contains(s, "outlook.office365.com") || strings.Contains(s, "imap.accessasuser") || strings.Contains(s, "smtp.send")
}

func graphMailContentScope(scope string) bool {
	s := strings.ToLower(scope)
	if strings.Contains(s, "mail.readbasic") {
		return false
	}
	return strings.Contains(s, "mail.read")
}

// microsoftGrantRequiresReauthorization reports grants that cannot read Graph
// mail. Non-JWT tokens are left for Graph to accept or reject. An Outlook
// audience, or a Graph token without Mail.Read/Mail.ReadWrite, cannot be
// fixed by refresh when the user never consented to those scopes.
func microsoftGrantRequiresReauthorization(accessToken string) error {
	aud, scp, ok := microsoftTokenAudienceAndScope(accessToken)
	if !ok {
		return nil
	}
	if audienceIsOutlook(aud) {
		return fmt.Errorf("microsoft mailbox grant is an Outlook IMAP token and cannot read Graph; reauthorize the mailbox for Microsoft Graph Mail.ReadWrite")
	}
	if audienceIsGraph(aud) && !scopeAllowsGraphMIME(scp) {
		return fmt.Errorf("microsoft mailbox grant does not include Graph Mail.ReadWrite; reauthorize the mailbox for Microsoft Graph Mail.ReadWrite")
	}
	return nil
}

func microsoftTokenAudienceAndScope(token string) (aud, scp string, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", "", false
		}
	}
	var claims struct {
		Aud   json.RawMessage `json:"aud"`
		Scp   string          `json:"scp"`
		Roles []string        `json:"roles"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", "", false
	}
	aud = decodeMicrosoftAudience(claims.Aud)
	scp = claims.Scp
	if scp == "" && len(claims.Roles) > 0 {
		scp = strings.Join(claims.Roles, " ")
	}
	if aud == "" && scp == "" {
		return "", "", false
	}
	return aud, scp, true
}

func decodeMicrosoftAudience(raw json.RawMessage) string {
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil && len(many) > 0 {
		return many[0]
	}
	return ""
}

func bytesTrimSpace(raw json.RawMessage) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(string(raw)))
}

func audienceIsOutlook(aud string) bool {
	a := strings.ToLower(strings.TrimSpace(aud))
	return strings.Contains(a, "outlook.office365.com") || a == "00000002-0000-0ff1-ce00-000000000000"
}

func audienceIsGraph(aud string) bool {
	a := strings.ToLower(strings.TrimSpace(aud))
	return strings.Contains(a, "graph.microsoft.com") || a == "00000003-0000-0000-c000-000000000000"
}

func scopeAllowsGraphMIME(scp string) bool {
	for _, part := range strings.Fields(scp) {
		if graphMailContentScope(part) {
			return true
		}
	}
	return false
}
