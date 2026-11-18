package graph

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"windshift/internal/models"
	"windshift/internal/outbound"
)

func TestSendThreadedEmailPreservesMIMEHeaders(t *testing.T) {
	var tokenHits atomic.Int32
	var sendBody string
	var sendType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			tokenHits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
		case strings.HasSuffix(r.URL.Path, "/sendMail"):
			sendType = r.Header.Get("Content-Type")
			raw, _ := io.ReadAll(r.Body)
			decoded, err := base64.StdEncoding.DecodeString(string(raw))
			if err != nil {
				t.Errorf("decode MIME body: %v", err)
				http.Error(w, "bad mime", http.StatusBadRequest)
				return
			}
			sendBody = string(decoded)
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sender := testSender(srv, models.ChannelConfig{
		GraphTenantID:     "tenant",
		GraphClientID:     "client",
		GraphClientSecret: "secret",
		GraphFromEmail:    "from@contoso.com",
		GraphFromName:     "Windshift",
	})
	err := sender.SendThreadedEmail(outbound.ThreadedEmailParams{
		ToEmail:      "customer@example.com",
		ToName:       "Lastname, Firstname",
		ReplyToEmail: "inbox@contoso.com",
		ReplyToName:  "Support",
		Subject:      "Re: ticket",
		HTMLBody:     "<p>html body</p>",
		TextBody:     "plain body",
		MessageID:    "<ws-1@contoso.com>",
		InReplyTo:    "prev@contoso.com",
		References:   []string{"<root@contoso.com>", "prev@contoso.com"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sendType != "text/plain" {
		t.Fatalf("content type %q, want text/plain", sendType)
	}
	for _, want := range []string{
		"Message-ID: <ws-1@contoso.com>",
		"In-Reply-To: <prev@contoso.com>",
		"References: <root@contoso.com> <prev@contoso.com>",
		"Reply-To:",
		"inbox@contoso.com",
		"text/plain",
		"text/html",
		"plain body",
		"<p>html body</p>",
		"Lastname, Firstname",
	} {
		if !strings.Contains(sendBody, want) {
			t.Errorf("MIME missing %q\n%s", want, sendBody)
		}
	}

	if err := sender.SendThreadedEmail(outbound.ThreadedEmailParams{
		ToEmail:  "customer@example.com",
		Subject:  "second",
		TextBody: "again",
		HTMLBody: "<p>again</p>",
	}); err != nil {
		t.Fatalf("second send: %v", err)
	}
	if got := tokenHits.Load(); got != 1 {
		t.Fatalf("token endpoint hits = %d, want 1 (cache unused)", got)
	}
}

func TestGraphRefusesTokenRedirect(t *testing.T) {
	var stolen atomic.Int32
	steal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stolen.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer steal.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, steal.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	sender := testSender(srv, graphConfig())
	err := sender.SendThreadedEmail(outbound.ThreadedEmailParams{ToEmail: "a@b.c", Subject: "s", TextBody: "t", HTMLBody: "h"})
	if err == nil || !strings.Contains(err.Error(), "refused redirect") {
		t.Fatalf("err = %v, want refused redirect", err)
	}
	if stolen.Load() != 0 {
		t.Fatalf("redirect target received %d requests", stolen.Load())
	}
}

func TestGraphSendHonorsCancellation(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	sender := testSender(srv, graphConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sender.SendThreadedEmailContext(ctx, outbound.ThreadedEmailParams{ToEmail: "a@b.c", Subject: "s", TextBody: "t"})
	if err == nil {
		t.Fatal("expected cancellation")
	}
	if hits.Load() != 0 {
		t.Fatalf("cancelled send hit the network %d times", hits.Load())
	}
}

func TestNewClientRejectsCleartextTokenURL(t *testing.T) {
	_, err := NewClient(Config{
		TenantID:     "tenant",
		ClientID:     "client",
		ClientSecret: "secret",
		TokenURL:     "http://login.evil.example/token",
	})
	if err == nil {
		t.Fatal("expected https requirement")
	}
}

func TestUnconfiguredGraphReturnsSentinel(t *testing.T) {
	s := NewSender(nil, "", nil, models.ChannelConfig{})
	err := s.SendTransactional("a@b.c", "en", "magic_link", nil)
	if err != outbound.ErrNotConfigured {
		t.Fatalf("err = %v, want outbound.ErrNotConfigured", err)
	}
}

func graphConfig() models.ChannelConfig {
	return models.ChannelConfig{
		GraphTenantID:     "tenant",
		GraphClientID:     "client-redirect",
		GraphClientSecret: "secret",
		GraphFromEmail:    "from@contoso.com",
	}
}

func testSender(srv *httptest.Server, cfg models.ChannelConfig) *Sender {
	s := NewSender(nil, "https://app.example", nil, cfg)
	s.testHTTP = srv.Client()
	s.testBaseURL = srv.URL
	s.testTokenURL = srv.URL + "/token"
	return s
}

