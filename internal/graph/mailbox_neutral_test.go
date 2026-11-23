package graph

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGraphMailboxPollReturnsNeutralMessages pins the boundary that keeps the
// Graph reader reusable outside email ingestion: what it hands back is a
// transport-neutral message, not an IMAP fetch result. The ref is the Graph
// message id, which is stable across polls and therefore safe to persist.
func TestGraphMailboxPollReturnsNeutralMessages(t *testing.T) {
	const (
		mailbox = "inbox@contoso.example"
		token   = "graph-token"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"value":[{"id":"AAMk-one","receivedDateTime":"2026-07-17T08:00:00Z"}],"@odata.deltaLink":"`+srvURL(r)+`/saved"}`)
		case strings.HasSuffix(r.URL.Path, "/$value"):
			w.Header().Set("Content-Type", "message/rfc822")
			_, _ = io.WriteString(w, "From: a@example.com\r\nSubject: hello\r\nMessage-ID: <m1@example.com>\r\n\r\nbody\r\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	box, err := OpenGraphMailbox(context.Background(), GraphMailboxConfig{
		BaseURL: srv.URL, Token: token, Mailbox: mailbox, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("OpenGraphMailbox: %v", err)
	}
	if err := box.SelectFolder("INBOX"); err != nil {
		t.Fatalf("SelectFolder: %v", err)
	}

	messages, cursor, err := box.PollMessages(context.Background(), GraphMailboxCursor{}, 10)
	if err != nil {
		t.Fatalf("PollMessages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}
	if got, want := messages[0].Ref, "AAMk-one"; got != want {
		t.Fatalf("ref = %q, want the Graph message id %q", got, want)
	}
	if !strings.Contains(string(messages[0].Raw), "Subject: hello") {
		t.Fatalf("raw payload missing headers: %q", messages[0].Raw)
	}
	if cursor.DeltaLink == "" {
		t.Fatal("cursor did not carry the delta link")
	}
}

// srvURL recovers the test server's own URL from the request it is serving,
// so the deltaLink points back at the same in-process server.
func srvURL(r *http.Request) string {
	scheme := "http"
	return scheme + "://" + r.Host
}

// TestGraphFallbackMessageIDFollowsGraphID pins that a message without a
// Message-ID still gets a stable, Graph-id-derived dedup identity.
func TestGraphFallbackMessageIDFollowsGraphID(t *testing.T) {
	raw := withGraphFallbackMessageID([]byte("Subject: Hi\r\n\r\nbody\r\n"), "AAMk/one")
	other := withGraphFallbackMessageID([]byte("Subject: Hi\r\n\r\nbody\r\n"), "AAMk/two")
	again := withGraphFallbackMessageID([]byte("Subject: Hi\r\n\r\nbody\r\n"), "AAMk/one")
	if bytes.Equal(raw, other) || !bytes.Equal(raw, again) {
		t.Fatalf("fallback ids are not stable per Graph id")
	}
	if !strings.Contains(string(raw), "AAMk/one") {
		t.Fatalf("fallback id missing the Graph id: %q", raw)
	}
}
