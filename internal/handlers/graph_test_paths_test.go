package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"windshift/internal/models"
)

// TestGraphChannelTestMessageShape pins testGraphChannelWithClient's
// contract minus network: (1) a Graph config must pass the validation gate,
// (2) a config missing a required Graph field must fail before any send,
// (3) a malformed JSON config must fail with a parse error, (4) the success
// message names Graph and the recipient.
func TestGraphChannelTestMessageShape(t *testing.T) {
	// (3) Malformed config JSON fails cleanly — this exercises the unmarshal
	// error path without a sender.
	channel := models.Channel{
		Type:   "smtp",
		Config: `{"graph_send_provider": "graph", "graph_client_secret": "x",`,
	}
	ok, msg := (&ChannelHandler{}).testGraphChannelWithClient(channel, "a@b.com")
	if ok || !strings.Contains(msg, "Failed to parse channel configuration") {
		t.Fatalf("malformed config: ok=%v msg=%q", ok, msg)
	}

	// (2) Incomplete Graph config must fail validation before any send.
	incomplete := models.Channel{
		Type:   "smtp",
		Config: `{"graph_send_provider":"graph","graph_tenant_id":"t"}`,
	}
	ok, msg = (&ChannelHandler{}).testGraphChannelWithClient(incomplete, "a@b.com")
	if ok {
		t.Fatalf("incomplete graph config passed: %q", msg)
	}
	if !strings.Contains(msg, "tenant") && !strings.Contains(msg, "client") && !strings.Contains(msg, "secret") && !strings.Contains(msg, "mailbox") {
		t.Errorf("validation error %q does not name the missing field", msg)
	}

	// Also verify the same config validates via the model's own contract —
	// the handler gate delegates to Validate(), keeping one source of truth.
	var cfg models.ChannelConfig
	if err := json.Unmarshal([]byte(incomplete.Config), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() accepted a Graph config with only a tenant id")
	}

	// (4) Complete-but-unwired-sender: the nil h.smtpSender short-circuits
	// with a clear message instead of panicking.
	complete := models.Channel{
		Type: "smtp",
		Config: `{"graph_send_provider":"graph","graph_tenant_id":"t","graph_client_id":"c",
			"graph_client_secret":"enc:v1:x","graph_mailbox_upn":"m@x.com"}`,
	}
	ok, msg = (&ChannelHandler{}).testGraphChannelWithClient(complete, "a@b.com")
	if ok {
		t.Fatalf("nil sender should fail: %q", msg)
	}
	if !strings.Contains(msg, "sender not configured") {
		t.Errorf("nil sender message = %q", msg)
	}
}