package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	windshiftsmtp "windshift/internal/smtp"

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

// handlerTestGraphSender mirrors smtp.GraphSender with local capture types —
// no import of smtp test fixtures.
type handlerTestGraphSender struct {
	sends []handlerCapturedSend
}

type handlerCapturedSend struct {
	Token, UPN string
	MIME       []byte
}

func (h *handlerTestGraphSender) FetchToken(ctx context.Context, tenantID, clientID, clientSecret string) (*windshiftsmtp.GraphToken, error) {
	return &windshiftsmtp.GraphToken{AccessToken: "fake-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (h *handlerTestGraphSender) SendMime(ctx context.Context, accessToken, mailboxUPN string, mime []byte) error {
	h.sends = append(h.sends, handlerCapturedSend{Token: accessToken, UPN: mailboxUPN, MIME: mime})
	return nil
}

// encFixedDecrypt implements models/email Encryptor with a fixed plaintext —
// the fake GraphSender in the smtp package accepted "plain-secret"; here it
// just needs to be non-empty through decryptOrLegacy.
type encFixedDecrypt struct{}

func (encFixedDecrypt) Decrypt(string) (string, error) { return "plain-secret", nil }
func (encFixedDecrypt) Encrypt(string) (string, error) { return "enc:v1:fixed", nil }

// TestGraphChannelTestSuccessPath proves a complete Graph config with a
// wired real NotificationSMTPSender + fake GraphSender delivers: the test
// goes through the genuine SendEmailWithConfig → sendEmailContext →
// dispatchContext → Graph branch chain, and buildMime must succeed (hence
// smtp_from_email present in the fixture).
func TestGraphChannelTestSuccessPath(t *testing.T) {
	graph := &handlerTestGraphSender{}
	sender := windshiftsmtp.NewNotificationSMTPSender(nil)
	sender.SetEncryption(encFixedDecrypt{})
	sender.SetGraphSender(graph)

	handler := &ChannelHandler{smtpSender: sender}
	channel := models.Channel{
		Type: "smtp",
		Config: `{"graph_send_provider":"graph","graph_tenant_id":"t","graph_client_id":"c",
			"graph_client_secret":"enc:v1:x","graph_mailbox_upn":"windshift@contoso.com",
			"smtp_from_email":"windshift@contoso.com","smtp_from_name":"Windshift"}`,
	}
	ok, msg := handler.testGraphChannelWithClient(channel, "dest@example.com")
	if !ok {
		t.Fatalf("expected success, got %q", msg)
	}
	if !strings.Contains(msg, "Microsoft Graph") || !strings.Contains(msg, "dest@example.com") {
		t.Errorf("success message = %q", msg)
	}
	if len(graph.sends) != 1 {
		t.Fatalf("graph sends = %d, want 1", len(graph.sends))
	}
	if graph.sends[0].UPN != "windshift@contoso.com" {
		t.Errorf("sendAs = %s", graph.sends[0].UPN)
	}
	if !strings.Contains(string(graph.sends[0].MIME), "To: dest@example.com") {
		t.Errorf("MIME missing recipient: %.200s", graph.sends[0].MIME)
	}
}
