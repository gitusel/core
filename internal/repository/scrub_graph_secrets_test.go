package repository

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestScrubChannelConfigRemovesGraphSecrets pins the redaction contract for
// the Graph send fields: the encrypted client secret and cached access token
// must never leave the app through a channel read, while the non-secret
// Graph fields (tenant, client id, mailbox UPN, provider) stay visible so
// the admin UI can render the config.
func TestScrubChannelConfigRemovesGraphSecrets(t *testing.T) {
	cfg := map[string]any{
		"graph_send_provider": "graph",
		"graph_tenant_id":     "tenant-abc",
		"graph_client_id":     "cid",
		"graph_client_secret": "encrypted-ciphertext",
		"graph_mailbox_upn":   "windshift@contoso.com",
		"graph_access_token":  "encrypted-token",
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	scrubbed := ScrubChannelConfig(string(raw))
	var out map[string]any
	if err := json.Unmarshal([]byte(scrubbed), &out); err != nil {
		t.Fatal(err)
	}

	for _, secret := range []string{"graph_client_secret", "graph_access_token"} {
		if _, ok := out[secret]; ok {
			t.Errorf("secret %q survived scrubbing", secret)
		}
	}
	for _, kept := range []string{"graph_send_provider", "graph_tenant_id", "graph_client_id", "graph_mailbox_upn"} {
		if _, ok := out[kept]; !ok {
			t.Errorf("visible field %q was removed", kept)
		}
	}
	// Defense in depth: no ciphertext fragment may leak anywhere in the output.
	if strings.Contains(scrubbed, "encrypted-ciphertext") || strings.Contains(scrubbed, "encrypted-token") {
		t.Errorf("scrubbed config still contains secret material: %s", scrubbed)
	}
}

func TestScrubChannelConfigKeepsLegacyFields(t *testing.T) {
	scrubbed := ScrubChannelConfig(`{"smtp_host":"smtp.office365.com","smtp_password":"pw","imap_password":"ipw"}`)
	var out map[string]any
	_ = json.Unmarshal([]byte(scrubbed), &out)
	if out["smtp_host"] != "smtp.office365.com" {
		t.Errorf("smtp_host lost")
	}
	if _, ok := out["smtp_password"]; ok {
		t.Errorf("smtp_password survived")
	}
}
