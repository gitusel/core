package database

import (
	"database/sql"
	"testing"
	"time"
)

// TestSSOStateNoncePersistsAndRoundTrips pins the DB half of the OIDC nonce
// binding: the StoreOIDC column set writes the nonce with the state token,
// the GetValid column set returns it on the callback lookup, and the SAML
// Store path leaves the field empty so its callback requires no token nonce.
//
// Direct SQL mirrors repository.NewSSOStateRepository's exact statements;
// importing the repository package from here would be an import cycle
// (repository -> database).
func TestSSOStateNoncePersistsAndRoundTrips(t *testing.T) {
	db, err := NewSQLiteDBWithPoolSizes(t.TempDir()+"/windshift.db", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Initialize(); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	// Satisfy the FK on provider_id.
	_, err = db.Exec(`INSERT INTO sso_providers (slug, name) VALUES ('oidc-test', 'Test IdP')`)
	if err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	var providerID int
	if err := db.QueryRow(`SELECT id FROM sso_providers WHERE slug='oidc-test'`).Scan(&providerID); err != nil {
		t.Fatal(err)
	}

	expires := time.Now().Add(5 * time.Minute)

	// OIDC path (StoreOIDC statement shape): nonce is written.
	_, err = db.Exec(`
		INSERT INTO sso_state_tokens (provider_id, state, nonce, redirect_uri, remember_me, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, providerID, "state-nonce", "the-nonce-value", "/redirect", true, expires)
	if err != nil {
		t.Fatalf("OIDC insert: %v", err)
	}

	// GetValid statement shape (same columns, order and scan targets as the
	// repository: request_id/nonce are nullable, so NullString mirrors the
	// repo's scan exactly).
	var requestID, nonce sql.NullString
	var redirectURI string
	var rememberMe bool
	err = db.QueryRow(`
		SELECT request_id, nonce, redirect_uri, remember_me FROM sso_state_tokens
		WHERE state = ? AND provider_id = ? AND expires_at > ?
	`, "state-nonce", providerID, time.Now()).Scan(&requestID, &nonce, &redirectURI, &rememberMe)
	if err != nil {
		t.Fatalf("GetValid-shaped read: %v", err)
	}
	if nonce.String != "the-nonce-value" {
		t.Fatalf("nonce persisted=%q, want the-nonce-value", nonce.String)
	}

	// SAML path (Store statement shape): nonce column not written → stays
	// NULL → reads back empty.
	_, err = db.Exec(`
		INSERT INTO sso_state_tokens (provider_id, state, request_id, redirect_uri, remember_me, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, providerID, "state-saml", "req-id-1", "/redirect", false, expires)
	if err != nil {
		t.Fatalf("SAML insert: %v", err)
	}
	err = db.QueryRow(`
		SELECT request_id, nonce, redirect_uri, remember_me FROM sso_state_tokens
		WHERE state = ? AND provider_id = ? AND expires_at > ?
	`, "state-saml", providerID, time.Now()).Scan(&requestID, &nonce, &redirectURI, &rememberMe)
	if err != nil {
		t.Fatalf("GetValid-shaped read (saml): %v", err)
	}
	if nonce.String != "" {
		t.Fatalf("SAML nonce must be empty, got %q", nonce.String)
	}

	// Concurrent logins persist independently — the callback binds each code
	// exchange to the nonce its own state row carries.
	for _, tc := range []struct{ state, nonce string }{{"state-a", "nonce-a"}, {"state-b", "nonce-b"}} {
		if _, err := db.Exec(`
			INSERT INTO sso_state_tokens (provider_id, state, nonce, redirect_uri, remember_me, expires_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, providerID, tc.state, tc.nonce, "/r", false, expires); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ state, nonce string }{{"state-a", "nonce-a"}, {"state-b", "nonce-b"}} {
		var got string
		if err := db.QueryRow(`SELECT nonce FROM sso_state_tokens WHERE state = ? AND provider_id = ?`, tc.state, providerID).Scan(&got); err != nil || got != tc.nonce {
			t.Fatalf("%s: nonce read %v (%q), want %q", tc.state, err, got, tc.nonce)
		}
	}
}
