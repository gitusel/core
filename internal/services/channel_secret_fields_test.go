package services

import (
	"testing"
)

// TestChannelSecretFieldsIncludeGraph pins the write-side encryption list:
// the Graph client secret and cached access token must be treated as
// channel secrets (encrypted before persistence, blank-keeps-stored on
// partial updates) exactly like smtp_password and the email OAuth secrets.
func TestChannelSecretFieldsIncludeGraph(t *testing.T) {
	for _, field := range []string{"graph_client_secret", "graph_access_token"} {
		if !isChannelSecretField(field) {
			t.Errorf("isChannelSecretField(%q) = false, want true", field)
		}
	}
	// Non-secret Graph fields must stay out of the list so they update freely.
	for _, field := range []string{"graph_tenant_id", "graph_client_id", "graph_mailbox_upn", "graph_send_provider"} {
		if isChannelSecretField(field) {
			t.Errorf("isChannelSecretField(%q) = true, want false", field)
		}
	}
}
