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

// TestMergeChannelConfigGraphSecretLifecycle pins the partial-update contract
// for the Graph secrets through mergeChannelConfig: blank keeps the stored
// ciphertext, a non-blank value replaces it, and explicit JSON null deletes
// it — matching how smtp_password and the email OAuth secrets behave.
func TestMergeChannelConfigGraphSecretLifecycle(t *testing.T) {
	const stored = `{"smtp_host":"smtp.office365.com","graph_client_secret":"stored-ciphertext","graph_access_token":"stored-token"}`

	// Blank Graph secret: keep the stored ciphertext.
	mergedBlank := map[string]any{"graph_client_secret": ""}
	res, _, err := mergeChannelConfig(stored, mergedBlank)
	if err != nil {
		t.Fatalf("blank merge: %v", err)
	}
	if got := res["graph_client_secret"]; got != "stored-ciphertext" {
		t.Errorf("blank: secret = %v, want stored ciphertext preserved", got)
	}

	// Non-blank replaces.
	mergedNew := map[string]any{"graph_client_secret": "new-ciphertext"}
	res, _, err = mergeChannelConfig(stored, mergedNew)
	if err != nil {
		t.Fatalf("replace merge: %v", err)
	}
	if got := res["graph_client_secret"]; got != "new-ciphertext" {
		t.Errorf("non-blank: secret = %v, want replaced", got)
	}

	// Explicit nil deletes.
	mergedNil := map[string]any{"graph_client_secret": nil}
	res, _, err = mergeChannelConfig(stored, mergedNil)
	if err != nil {
		t.Fatalf("null merge: %v", err)
	}
	if _, ok := res["graph_client_secret"]; ok {
		t.Errorf("null: secret key still present, want deleted")
	}
}
