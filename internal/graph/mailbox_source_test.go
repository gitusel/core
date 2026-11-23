package graph

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"time"
	"windshift/internal/mailbox"
)

// TestGraphMailboxImplementsMailbox pins the parity that lets one poller drive
// both transports: the Graph mailbox exposes exactly the neutral surface, with
// no UID space, no SelectData, and no expunge.
func TestGraphMailboxImplementsMailbox(t *testing.T) {
	const (
		mailboxAddr = "box@contoso.example"
		token       = "tok"
	)
	var reads, deletes int
	var body string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"value":[{"id":"AAMk-a","receivedDateTime":"2026-07-17T08:00:00Z"}],"@odata.deltaLink":"`+srv.URL+`/saved"}`)
		case strings.HasSuffix(r.URL.Path, "/$value"):
			w.Header().Set("Content-Type", "message/rfc822")
			_, _ = io.WriteString(w, "From: c@example.com\r\nSubject: s\r\nMessage-ID: <m@example.com>\r\n\r\nbody\r\n")
		case r.Method == http.MethodPatch:
			reads++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete:
			deletes++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	native, err := OpenGraphMailbox(context.Background(), GraphMailboxConfig{
		BaseURL: srv.URL, Token: token, Mailbox: mailboxAddr, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("OpenGraphMailbox: %v", err)
	}
	var mb mailbox.Mailbox = NewGraphMailbox(native)

	info, err := mb.Open(context.Background(), "INBOX", false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if info.HasExisting {
		t.Fatal("a delta mailbox has no history to skip; HasExisting must stay false")
	}
	if mb.Checkpoint() != "" {
		t.Fatalf("fresh checkpoint = %q, want empty", mb.Checkpoint())
	}

	deliveries, err := mb.Fetch(context.Background(), 10)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Ref != "AAMk-a" {
		t.Fatalf("deliveries = %#v, want one ref AAMk-a", deliveries)
	}
	// The round is fetched but not yet acknowledged: the checkpoint must still
	// point at the pre-fetch state so an early exit replays this batch rather
	// than skipping it.
	if got := mb.Checkpoint(); got != "" {
		t.Fatalf("checkpoint before acknowledge = %q, want empty", got)
	}
	mb.Acknowledge()
	if got := mb.Checkpoint(); !strings.Contains(got, "/saved") {
		t.Fatalf("checkpoint = %q, want it to carry the delta link", got)
	}

	if err := mb.MarkRead(context.Background(), "AAMk-a"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if err := mb.Delete(context.Background(), "AAMk-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if reads != 1 || deletes != 1 {
		t.Fatalf("reads=%d deletes=%d, want 1/1 addressed by Graph id", reads, deletes)
	}
	// Graph delete is immediate; expunge must be a no-op that cannot fail.
	if err := mb.Expunge(context.Background()); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	_ = body
}

// TestGraphMailboxCheckpointIsStable proves the persisted value is the delta
// link, not a per-poll ordinal, so restoring it resumes the same round.
func TestGraphMailboxCheckpointIsStable(t *testing.T) {
	native, err := OpenGraphMailbox(context.Background(), GraphMailboxConfig{
		BaseURL: "http://127.0.0.1:1", Token: "t", Mailbox: "b@example.com",
	})
	if err != nil {
		t.Fatalf("OpenGraphMailbox: %v", err)
	}
	mb := NewGraphMailbox(native)
	mb.Restore("https://graph.microsoft.com/v1.0/users/b/mailFolders/inbox/messages/delta?$deltatoken=x")
	if got := mb.Checkpoint(); !strings.Contains(got, "$deltatoken=x") {
		t.Fatalf("restored checkpoint = %q", got)
	}
}

// TestGraphMailboxHonorsBaselineCutoff pins that a mailbox primed with Seed does
// not re-ingest the history the baseline skipped.
func TestGraphMailboxHonorsBaselineCutoff(t *testing.T) {
	const mailboxAddr = "box@contoso.example"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			w.Header().Set("Content-Type", "application/json")
			// One message from before the cutoff, one from after.
			_, _ = io.WriteString(w, `{"value":[
				{"id":"OLD","receivedDateTime":"2020-01-01T00:00:00Z"},
				{"id":"NEW","receivedDateTime":"2030-01-01T00:00:00Z"}],
				"@odata.deltaLink":"`+srv.URL+`/saved"}`)
		case strings.HasSuffix(r.URL.Path, "/$value"):
			w.Header().Set("Content-Type", "message/rfc822")
			_, _ = io.WriteString(w, "From: c@example.com\r\nSubject: s\r\nMessage-ID: <m@example.com>\r\n\r\nbody\r\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	native, err := OpenGraphMailbox(context.Background(), GraphMailboxConfig{
		BaseURL: srv.URL, Token: "t", Mailbox: mailboxAddr, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("OpenGraphMailbox: %v", err)
	}
	mb := NewGraphMailbox(native).(*graphMailbox)
	if _, err := mb.Open(context.Background(), "INBOX", false); err != nil {
		t.Fatalf("Open: %v", err)
	}
	mb.checkpoint.NotBefore = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	deliveries, err := mb.Fetch(context.Background(), 10)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Ref != "NEW" {
		t.Fatalf("deliveries = %#v, want only the post-cutoff message", deliveries)
	}
}

// TestGraphMailboxCheckpointAdvancesOnlyOnAcknowledge mirrors the IMAP contract:
// a batch that stops early must not persist a cursor past unprocessed mail.
func TestGraphMailboxCheckpointAdvancesOnlyOnAcknowledge(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages/delta") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"value":[],"@odata.deltaLink":"`+srv.URL+`/next"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	native, err := OpenGraphMailbox(context.Background(), GraphMailboxConfig{
		BaseURL: srv.URL, Token: "t", Mailbox: "b@example.com", HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("OpenGraphMailbox: %v", err)
	}
	mb := NewGraphMailbox(native)
	if _, err := mb.Open(context.Background(), "INBOX", false); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := mb.Fetch(context.Background(), 10); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := mb.Checkpoint(); got != "" {
		t.Fatalf("checkpoint before acknowledge = %q, want empty", got)
	}
	mb.Acknowledge()
	if got := mb.Checkpoint(); !strings.Contains(got, "/next") {
		t.Fatalf("checkpoint after acknowledge = %q, want the new link", got)
	}
}
