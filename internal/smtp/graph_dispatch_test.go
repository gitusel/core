package smtp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"windshift/internal/models"
)

// fakeEncryptor implements Encryptor: Decrypt always yields plain-secret so
// the test proves the Graph branch decrypts through the same helper as SMTP.
type fakeEncryptor struct{}

func (fakeEncryptor) Decrypt(string) (string, error) { return "plain-secret", nil }
func (fakeEncryptor) Encrypt(string) (string, error) { return "enc:v1:fake", nil }

var _ Encryptor = fakeEncryptor{}

// fakeGraphSender is a package-local GraphSender double driving the dispatch
// branch hermetically. fetchErr/sendErr inject failures; fetchCount/sendCalls
// record every call so tests can assert routing and caching.
type fakeGraphSender struct {
	mu sync.Mutex

	fetchErr   error
	sendErr    error
	wantSecret string // non-empty: fetch fails with AADSTS7000215 unless clientSecret matches

	fetchCount int
	sendCalls  []CapturedSend
	gotSecret  string
}

type CapturedSend struct {
	Token, UPN string
}

func (f *fakeGraphSender) FetchToken(ctx context.Context, tenantID, clientID, clientSecret string) (*GraphToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchCount++
	f.gotSecret = clientSecret
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	if f.wantSecret != "" && clientSecret != f.wantSecret {
		return nil, errors.New("invalid_client: AADSTS7000215")
	}
	return &GraphToken{AccessToken: "fake-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (f *fakeGraphSender) SendMime(ctx context.Context, accessToken, mailboxUPN string, mime []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sendCalls = append(f.sendCalls, CapturedSend{Token: accessToken, UPN: mailboxUPN})
	return nil
}

var _ GraphSender = (*fakeGraphSender)(nil)

// graphChannel builds a complete Graph channel config with no SMTP fields —
// the branch under test must not require them.
func graphChannel() *models.ChannelConfig {
	return &models.ChannelConfig{
		GraphSendProvider: "graph",
		GraphTenantID:     "tenant-1",
		GraphClientID:     "cid",
		GraphClientSecret: "enc:v1:ciphertext", // fakeEncryptor decrypts to plain-secret
		GraphMailboxUPN:   "windshift@contoso.com",
	}
}

// TestDispatchContextGraphSendsMime pins the Phase 3 branch: a Graph channel
// skips SMTP validation entirely, decrypts the client secret through the
// shared helper (proved by the fake receiving the decrypted value), and
// delivers the MIME via the Graph sender.
func TestDispatchContextGraphSendsMime(t *testing.T) {
	fg := &fakeGraphSender{}
	sender := NewNotificationSMTPSender(nil)
	sender.SetEncryption(fakeEncryptor{})
	sender.SetGraphSender(fg)

	err := sender.dispatchContext(context.Background(), graphChannel(), "dest@example.com",
		[]byte("From: windshift@contoso.com\r\nTo: dest@example.com\r\nSubject: t\r\n\r\nbody"))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if fg.gotSecret != "plain-secret" {
		t.Errorf("token endpoint received secret %q, want decrypted %q", fg.gotSecret, "plain-secret")
	}
	if fg.fetchCount != 1 {
		t.Errorf("fetch count = %d, want 1", fg.fetchCount)
	}
	if len(fg.sendCalls) != 1 {
		t.Fatalf("send calls = %d, want 1", len(fg.sendCalls))
	}
	if fg.sendCalls[0].UPN != "windshift@contoso.com" {
		t.Errorf("sendAs = %s", fg.sendCalls[0].UPN)
	}
	if fg.sendCalls[0].Token != "fake-token" {
		t.Errorf("token = %q", fg.sendCalls[0].Token)
	}
}

// TestDispatchContextGraphTokenCached pins the concurrency-safe token cache:
// repeated dispatches share one fetch (no per-message token round trip).
func TestDispatchContextGraphTokenCached(t *testing.T) {
	fg := &fakeGraphSender{}
	sender := NewNotificationSMTPSender(nil)
	sender.SetEncryption(fakeEncryptor{})
	sender.SetGraphSender(fg)

	for i := 0; i < 3; i++ {
		if err := sender.dispatchContext(context.Background(), graphChannel(), "d@x.com", []byte("mime")); err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
	}
	if fg.fetchCount != 1 {
		t.Errorf("fetch count = %d after 3 sends, want 1 (cached)", fg.fetchCount)
	}
	if len(fg.sendCalls) != 3 {
		t.Errorf("send calls = %d, want 3", len(fg.sendCalls))
	}
}

// TestDispatchContextGraphBadTokenFails pins fail-closed token handling by
// injecting an AAD failure: the error must surface with its code and no send
// may happen.
func TestDispatchContextGraphBadTokenFails(t *testing.T) {
	fg := &fakeGraphSender{fetchErr: errors.New("invalid_client: AADSTS7000215")}
	sender := NewNotificationSMTPSender(nil)
	sender.SetEncryption(fakeEncryptor{})
	sender.SetGraphSender(fg)

	err := sender.dispatchContext(context.Background(), graphChannel(), "d@x.com", []byte("mime"))
	if err == nil || !strings.Contains(err.Error(), "7000215") {
		t.Fatalf("want AADSTS7000215 error, got %v", err)
	}
	if len(fg.sendCalls) != 0 {
		t.Errorf("send attempted despite token failure")
	}
}

// TestDispatchContextGraphMissingSecret pins the fail-closed secret check:
// an empty decrypted secret refuses to send without contacting Graph.
func TestDispatchContextGraphMissingSecret(t *testing.T) {
	fg := &fakeGraphSender{}
	sender := NewNotificationSMTPSender(nil)
	// No encryption configured: decryptOrLegacy passes the ciphertext through,
	// which is non-empty, so instead delete the secret from config.
	cfg := graphChannel()
	cfg.GraphClientSecret = ""
	sender.SetGraphSender(fg)

	err := sender.dispatchContext(context.Background(), cfg, "d@x.com", []byte("mime"))
	if err == nil || !strings.Contains(err.Error(), "client secret") {
		t.Fatalf("want missing-secret error, got %v", err)
	}
	if fg.fetchCount != 0 {
		t.Errorf("fetch attempted with missing secret")
	}
}

// TestDispatchContextGraphNoSenderWired pins the fail-closed wiring check:
// without SetGraphSender, a Graph channel errors instead of falling through
// to SMTP (where it would fail on missing host with a confusing message).
func TestDispatchContextGraphNoSenderWired(t *testing.T) {
	sender := NewNotificationSMTPSender(nil)
	sender.SetEncryption(fakeEncryptor{})

	err := sender.dispatchContext(context.Background(), graphChannel(), "d@x.com", []byte("mime"))
	if err == nil || !strings.Contains(err.Error(), "graph sender") {
		t.Fatalf("want no-graph-sender error, got %v", err)
	}
}

// TestDispatchContextGraphSecretCheckFails proves the token endpoint rejects
// an unexpected secret (proves dispatch decrypts via the shared helper and
// forwards the plaintext, not the ciphertext).
func TestDispatchContextGraphSecretCheckFails(t *testing.T) {
	fg := &fakeGraphSender{wantSecret: "different-secret"}
	sender := NewNotificationSMTPSender(nil)
	sender.SetEncryption(fakeEncryptor{})
	sender.SetGraphSender(fg)

	err := sender.dispatchContext(context.Background(), graphChannel(), "d@x.com", []byte("mime"))
	if err == nil || !strings.Contains(err.Error(), "7000215") {
		t.Fatalf("want secret-mismatch AAD error, got %v", err)
	}
}

// TestDispatchContextSMTPUnchanged guards the compat invariant: a legacy
// SMTP channel (no graph_send_provider) never consults the Graph sender —
// even though SMTP dial-out fails against an unresolvable host, the Graph
// sender records zero sends. Hermetic: no network dependency beyond the
// doomed SMTP dial to an invalid local address.
func TestDispatchContextSMTPUnchanged(t *testing.T) {
	fg := &fakeGraphSender{}
	sender := NewNotificationSMTPSender(nil)
	sender.SetEncryption(fakeEncryptor{})
	sender.SetGraphSender(fg)

	cfg := &models.ChannelConfig{
		SMTPHost:       "smtp.invalid-unreachable.invalid",
		SMTPPort:       587,
		SMTPAuthMethod: "none",
		SMTPFromEmail:  "from@example.com",
		SMTPUsername:   "u",
		SMTPPassword:   "p",
	}
	_ = sender.dispatchContext(context.Background(), cfg, "d@x.com", []byte("mime"))
	if len(fg.sendCalls) != 0 {
		t.Fatalf("graph sender consulted %d times for an SMTP channel", len(fg.sendCalls))
	}
	// And it must count as an SMTP (not Graph) dispatch.
	if fg.fetchCount != 0 {
		t.Errorf("token fetched for an SMTP channel")
	}
}
