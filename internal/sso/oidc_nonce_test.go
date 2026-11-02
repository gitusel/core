package sso

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-jose/go-jose/v4"
	"github.com/zitadel/oidc/v3/pkg/client/rp"
	httphelper "github.com/zitadel/oidc/v3/pkg/http"
	"golang.org/x/oauth2"
)

// passthroughRP is a minimal rp.RelyingParty whose OAuthConfig produces auth
// URLs deterministically; nil CookieHandler makes AuthURLHandler skip state
// cookies, and IsPKCE=false skips the code-challenge wiring. Only what
// AuthURLHandler touches is implemented.
type passthroughRP struct {
	cfg *oauth2.Config
}

func (p *passthroughRP) OAuthConfig() *oauth2.Config { return p.cfg }
func (p *passthroughRP) Issuer() string              { return "https://issuer.example" }
func (p *passthroughRP) IsPKCE() bool                { return false }
func (p *passthroughRP) CookieHandler() *httphelper.CookieHandler {
	return nil
}
func (p *passthroughRP) HttpClient() *http.Client               { return http.DefaultClient }
func (p *passthroughRP) IsOAuth2Only() bool                     { return false }
func (p *passthroughRP) Signer() jose.Signer                    { return nil }
func (p *passthroughRP) GetEndSessionEndpoint() string          { return "" }
func (p *passthroughRP) GetRevokeEndpoint() string              { return "" }
func (p *passthroughRP) UserinfoEndpoint() string               { return "" }
func (p *passthroughRP) GetDeviceAuthorizationEndpoint() string { return "" }
func (p *passthroughRP) IDTokenVerifier() *rp.IDTokenVerifier   { return nil }
func (p *passthroughRP) ErrorHandler() func(http.ResponseWriter, *http.Request, string, string, string) {
	return nil
}
func (p *passthroughRP) Logger(context.Context) (*slog.Logger, bool) { return nil, false }

func newTestRP() rp.RelyingParty {
	return &passthroughRP{cfg: &oauth2.Config{
		ClientID:    "client-id",
		Endpoint:    oauth2.Endpoint{AuthURL: "https://issuer.example/authorize", AuthStyle: oauth2.AuthStyleInHeader},
		RedirectURL: "https://sp.example/callback",
		Scopes:      []string{"openid"},
	}}
}

// TestGetAuthURLHandlerSendsNonce pins the authorization-URL half of the
// nonce flow: a nonce passed to GetAuthURLHandler must appear as the `nonce`
// authorization parameter in the redirect, so the IdP embeds it in the ID
// token it issues for this login. An empty nonce (the SAML/unwired path)
// must not add the parameter at all — zitadel's default then also requires
// an absent token nonce at the callback.
func TestGetAuthURLHandlerSendsNonce(t *testing.T) {
	svc := NewOIDCService([]byte("0123456789abcdef0123456789abcdef"))
	testRP := newTestRP()

	tests := []struct {
		name      string
		nonce     string
		wantInURL bool
	}{
		{"nonce sent when provided", "login-nonce-abc123", true},
		{"nonce omitted when empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := svc.GetAuthURLHandler(testRP, func() string { return "state-xyz" }, tt.nonce)
			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodGet, "/login", http.NoBody))

			if rec.Code != http.StatusFound {
				t.Fatalf("want 302, got %d", rec.Code)
			}
			location := rec.Header().Get("Location")
			if !strings.Contains(location, "state=state-xyz") {
				t.Fatalf("state missing from auth URL: %s", location)
			}
			if got := strings.Contains(location, "nonce=login-nonce-abc123"); got != tt.wantInURL {
				t.Errorf("nonce param present=%v, want %v (URL: %s)", got, tt.wantInURL, location)
			}
		})
	}
}

// TestWithExpectedNonceRoundtrip pins the context contract used by the
// callback: the ID-token verifier reads the nonce via
// rp.WithNonce(expectedNonce), whose return value decides whetherzitadel's
// CheckNonce enforces equality. A plain (un-patched) context yields "" —
// zitadel's default — which is what keeps call sites that do not set the
// nonce compiling unchanged.
func TestWithExpectedNonceRoundtrip(t *testing.T) {
	if got := expectedNonce(WithExpectedNonce(t.Context(), "n-42")); got != "n-42" {
		t.Fatalf("expected nonce n-42, got %q", got)
	}
	if got := expectedNonce(t.Context()); got != "" {
		t.Fatalf("expected empty nonce on plain context, got %q", got)
	}
}

// TestConcurrentLoginsIsolateExpectedNonces pins the per-request isolation
// claim: parallel in-flight callback requests carrying different expected
// nonces must each read back exactly their own value. A shared or
// process-global nonce would let one login's replayed ID token satisfy
// another login's verification.
func TestConcurrentLoginsIsolateExpectedNonces(t *testing.T) {
	const parallel = 50
	var wg sync.WaitGroup
	errs := make(chan error, parallel)

	for i := range parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := "nonce-for-login-" + strconv.Itoa(i)
			ctx := WithExpectedNonce(t.Context(), want)
			if got := expectedNonce(ctx); got != want {
				errs <- fmt.Errorf("login %d: expected nonce %q, got %q", i, want, got)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
