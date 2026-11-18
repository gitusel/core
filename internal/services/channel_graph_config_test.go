package services

import (
	"strings"
	"testing"

	"windshift/internal/models"
)

func TestGraphSecretEncryptBlankPreserveAndIdentityChange(t *testing.T) {
	svc := &ChannelConfigUpdateService{secret: func(value string) (string, error) {
		return "enc:" + value, nil
	}}
	incoming := map[string]any{
		"graph_tenant_id":     "tenant",
		"graph_client_id":     "client",
		"graph_client_secret": "plain-secret",
		"graph_from_email":    "box@contoso.example",
	}
	if err := svc.encryptSecrets(incoming); err != nil {
		t.Fatal(err)
	}
	if incoming["graph_client_secret"] != "enc:plain-secret" {
		t.Fatalf("encrypted secret = %#v", incoming["graph_client_secret"])
	}

	stored := `{"graph_tenant_id":"tenant","graph_client_id":"client","graph_client_secret":"enc:stored","graph_from_email":"box@contoso.example"}`
	merged, _, err := mergeChannelConfig(stored, map[string]any{"graph_client_secret": ""})
	if err != nil {
		t.Fatal(err)
	}
	if merged["graph_client_secret"] != "enc:stored" {
		t.Fatalf("blank edit replaced stored secret with %#v", merged["graph_client_secret"])
	}

	merged, _, err = mergeChannelConfig(stored, map[string]any{
		"graph_client_id":     "other-client",
		"graph_client_secret": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := merged["graph_client_secret"]; ok {
		t.Fatalf("identity change kept the previous secret: %#v", merged["graph_client_secret"])
	}
}

func TestValidateGraphEnableRequiresGraphFields(t *testing.T) {
	err := validateGraphEnable(&models.ChannelConfig{
		GraphTenantID:     "tenant",
		GraphClientID:     "client",
		GraphClientSecret: "enc:stored",
	})
	if err == nil || !strings.Contains(err.Error(), "from address") {
		t.Fatalf("missing from address: %v", err)
	}

	channel := &models.Channel{Type: "graph", Status: "enabled"}
	err = validateEnabledChannel(channel, &models.ChannelConfig{
		GraphTenantID:     "tenant",
		GraphClientID:     "client",
		GraphClientSecret: "enc:stored",
		GraphFromEmail:    "not-an-email",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "bare email") {
		t.Fatalf("invalid from address: %v", err)
	}

	err = validateEnabledChannel(channel, &models.ChannelConfig{
		GraphTenantID:     "tenant",
		GraphClientID:     "client",
		GraphClientSecret: "enc:stored",
		GraphFromEmail:    "box@contoso.example",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}
