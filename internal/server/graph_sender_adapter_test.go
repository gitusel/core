package server

import (
	"testing"
	"time"

	"windshift/internal/email"
)

// TestGraphTokenFromOAuthPresentExpiry pins the value mapping when the wire
// carries expires_in.
func TestGraphTokenFromOAuthPresentExpiry(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour)
	out, err := graphTokenFromOAuth(&email.OAuthTokens{
		AccessToken: "tok",
		ExpiresAt:   &exp,
	})
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if out.AccessToken != "tok" {
		t.Errorf("token = %q", out.AccessToken)
	}
	if !out.ExpiresAt.Equal(exp) {
		t.Errorf("expiry = %v, want %v", out.ExpiresAt, exp.UTC())
	}
}

// TestGraphTokenFromOAuthNilExpiry pins the zero-expiry rule: a token of
// unknown lifetime must map to zero time so the sender's cache refetches
// every send instead of reusing the token blindly.
func TestGraphTokenFromOAuthNilExpiry(t *testing.T) {
	out, err := graphTokenFromOAuth(&email.OAuthTokens{AccessToken: "tok"})
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if !out.ExpiresAt.IsZero() {
		t.Errorf("nil expiry mapped to %v, want zero time", out.ExpiresAt)
	}
}

// TestGraphTokenFromOAuthRejectsInvalid pins the fail-closed mapping: nil
// responses and empty tokens must error, never produce a sendable token.
func TestGraphTokenFromOAuthRejectsInvalid(t *testing.T) {
	if _, err := graphTokenFromOAuth(nil); err == nil {
		t.Error("nil token response accepted")
	}
	if _, err := graphTokenFromOAuth(&email.OAuthTokens{}); err == nil {
		t.Error("empty access token accepted")
	}
}
