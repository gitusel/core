package email

import (
	"context"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// stubIMAP is a minimal IMAPClient recording what the mailbox wrapper asks of it.
type stubIMAP struct {
	selected  string
	sinceUIDs []uint32
	fetched   []*FetchedMessage
	marked    []uint32
	deleted   []uint32
	expunged  int
	closed    bool
}

func (s *stubIMAP) SelectMailbox(name string) (*imap.SelectData, error) {
	s.selected = name
	return &imap.SelectData{UIDValidity: 7, UIDNext: 42}, nil
}
func (s *stubIMAP) FetchMessages(sinceUID uint32, _ int) ([]*FetchedMessage, error) {
	s.sinceUIDs = append(s.sinceUIDs, sinceUID)
	return s.fetched, nil
}
func (s *stubIMAP) MarkAsRead(uid uint32) error    { s.marked = append(s.marked, uid); return nil }
func (s *stubIMAP) DeleteMessage(uid uint32) error { s.deleted = append(s.deleted, uid); return nil }
func (s *stubIMAP) Expunge() error                 { s.expunged++; return nil }
func (s *stubIMAP) Close() error                   { s.closed = true; return nil }

// TestIMAPMailboxKeepsUIDsInside pins the neutrality of the Mailbox contract:
// the caller sees refs as opaque strings, while the IMAP implementation keeps
// the UID space, UIDVALIDITY, and UIDNEXT to itself. This is what later lets a
// Graph or Teams mailbox implement the same interface without inventing UIDs.
func TestIMAPMailboxKeepsUIDsInside(t *testing.T) {
	c := &stubIMAP{fetched: []*FetchedMessage{
		{UID: 12, Raw: []byte("Subject: a\r\n\r\nbody\r\n")},
		{UID: 13, Raw: []byte("Subject: b\r\n\r\nbody\r\n")},
	}}
	mb := NewIMAPMailbox(c)

	info, err := mb.Open(context.Background(), "INBOX", false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !info.HasExisting {
		t.Fatal("HasExisting should be true when the server reports UIDNEXT > 1")
	}
	if c.selected != "INBOX" {
		t.Fatalf("selected %q", c.selected)
	}

	deliveries, err := mb.Fetch(context.Background(), 10)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("got %d deliveries", len(deliveries))
	}
	if got, want := deliveries[0].Ref, "12"; got != want {
		t.Fatalf("ref = %q, want the decimal UID %q", got, want)
	}
	if len(c.sinceUIDs) != 1 || c.sinceUIDs[0] != 0 {
		t.Fatalf("first fetch sinceUID = %v, want 0", c.sinceUIDs)
	}

	if err := mb.MarkRead(context.Background(), "13"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if len(c.marked) != 1 || c.marked[0] != 13 {
		t.Fatalf("marked = %v, want [13]", c.marked)
	}
	if err := mb.Delete(context.Background(), "12"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(c.deleted) != 1 || c.deleted[0] != 12 {
		t.Fatalf("deleted = %v, want [12]", c.deleted)
	}
	if err := mb.Expunge(context.Background()); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	if c.expunged != 1 {
		t.Fatalf("expunged %d times", c.expunged)
	}
	mb.Acknowledge()
	if _, err := mb.Fetch(context.Background(), 10); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if c.sinceUIDs[1] != 13 {
		t.Fatalf("second fetch sinceUID = %d, want the highest consumed UID 13", c.sinceUIDs[1])
	}
	if err := mb.Close(); err != nil || !c.closed {
		t.Fatalf("Close: err=%v closed=%v", err, c.closed)
	}
}

// TestIMAPMailboxRejectsBadRef keeps the parse boundary honest: a ref that is
// not a UID is a programming error, not a silent no-op.
func TestIMAPMailboxRejectsBadRef(t *testing.T) {
	mb := NewIMAPMailbox(&stubIMAP{})
	if err := mb.MarkRead(context.Background(), "not-a-uid"); err == nil {
		t.Fatal("MarkRead accepted a non-numeric ref")
	}
}

// TestIMAPMailboxCheckpointWaitsForAcknowledge pins the cursor contract: a batch
// that was fetched but not committed must not move the persisted cursor, or a
// poll that stops early would skip the messages it never processed.
func TestIMAPMailboxCheckpointWaitsForAcknowledge(t *testing.T) {
	c := &stubIMAP{fetched: []*FetchedMessage{
		{UID: 20, Raw: []byte("Subject: a\r\n\r\nbody\r\n")},
		{UID: 21, Raw: []byte("Subject: b\r\n\r\nbody\r\n")},
	}}
	mb := NewIMAPMailbox(c)
	if _, err := mb.Open(context.Background(), "INBOX", false); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := mb.Checkpoint(); got != "" {
		t.Fatalf("fresh checkpoint = %q, want empty", got)
	}
	if _, err := mb.Fetch(context.Background(), 10); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := mb.Checkpoint(); got != "" {
		t.Fatalf("checkpoint after unacknowledged fetch = %q, want empty: an early exit must not persist a cursor past unprocessed mail", got)
	}
	mb.Acknowledge()
	if got := mb.Checkpoint(); !strings.Contains(got, "21") {
		t.Fatalf("checkpoint after acknowledge = %q, want it to carry UID 21", got)
	}
}

// TestIMAPMailboxResumeFromCheckpoint proves the round-trip the scheduler relies
// on: persisting the checkpoint and restoring it resumes at the same UID.
func TestIMAPMailboxResumeFromCheckpoint(t *testing.T) {
	c := &stubIMAP{fetched: []*FetchedMessage{{UID: 30, Raw: []byte("Subject: a\r\n\r\nbody\r\n")}}}
	first := NewIMAPMailbox(c)
	if _, err := first.Open(context.Background(), "INBOX", false); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := first.Fetch(context.Background(), 10); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	first.Acknowledge()
	saved := first.Checkpoint()

	second := NewIMAPMailbox(c)
	if _, err := second.Open(context.Background(), "INBOX", false); err != nil {
		t.Fatalf("Open: %v", err)
	}
	second.Restore(saved)
	if _, err := second.Fetch(context.Background(), 10); err != nil {
		t.Fatalf("resume fetch: %v", err)
	}
	if got := c.sinceUIDs[len(c.sinceUIDs)-1]; got != 30 {
		t.Fatalf("resumed sinceUID = %d, want 30", got)
	}
}

// TestIMAPMailboxSkipsExistingHistory pins the first-poll rule: a new intake
// that does not import history must start after the newest existing message.
// The UIDNEXT arithmetic stays inside the mailbox so the scheduler never sees a
// UID.
func TestIMAPMailboxSkipsExistingHistory(t *testing.T) {
	c := &stubIMAP{}
	mb := NewIMAPMailbox(c)
	info, err := mb.Open(context.Background(), "INBOX", true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !info.HasExisting {
		t.Fatal("HasExisting should be true for a mailbox with UIDNEXT 42")
	}
	if info.Validity != 7 {
		t.Fatalf("validity = %d, want the server's UIDVALIDITY 7", info.Validity)
	}
	if _, err := mb.Fetch(context.Background(), 10); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(c.sinceUIDs) != 1 || c.sinceUIDs[0] != 41 {
		t.Fatalf("first fetch sinceUID = %v, want 41 (UIDNEXT-1)", c.sinceUIDs)
	}
}

// TestIMAPMailboxImportsExistingHistory keeps the opt-in path honest: when the
// operator wants history, the first fetch starts at UID 0.
func TestIMAPMailboxImportsExistingHistory(t *testing.T) {
	c := &stubIMAP{}
	mb := NewIMAPMailbox(c)
	if _, err := mb.Open(context.Background(), "INBOX", false); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := mb.Fetch(context.Background(), 10); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if c.sinceUIDs[0] != 0 {
		t.Fatalf("first fetch sinceUID = %d, want 0 to import history", c.sinceUIDs[0])
	}
}

// changingValidityIMAP reports a UIDVALIDITY that changes after Open, modelling
// a server that renumbered the mailbox.
type changingValidityIMAP struct {
	stubIMAP
	validity uint32
	next     uint32
}

func (c *changingValidityIMAP) SelectMailbox(name string) (*imap.SelectData, error) {
	c.selected = name
	return &imap.SelectData{UIDValidity: c.validity, UIDNext: imap.UID(c.next)}, nil
}

// TestIMAPMailboxResetsOnUIDValidityChange pins the renumbering guard: a stored
// watermark from the old UID space must not survive a UIDVALIDITY change, or
// mail is skipped for good.
func TestIMAPMailboxResetsOnUIDValidityChange(t *testing.T) {
	c := &changingValidityIMAP{validity: 100, next: 500}
	mb := NewIMAPMailbox(c)
	if _, err := mb.Open(context.Background(), "INBOX", false); err != nil {
		t.Fatalf("Open: %v", err)
	}
	c.fetched = []*FetchedMessage{{UID: 480, Raw: []byte("Subject: a\r\n\r\nbody\r\n")}}
	if _, err := mb.Fetch(context.Background(), 10); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	mb.Acknowledge()
	if got := mb.Checkpoint(); !strings.Contains(got, "100:480") {
		t.Fatalf("checkpoint = %q, want validity 100 and UID 480", got)
	}

	// The server renumbers the mailbox.
	c.validity = 200
	c.next = 3
	mb.Restore(mb.Checkpoint())
	if _, err := mb.Open(context.Background(), "INBOX", false); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := mb.Checkpoint(); got != "" {
		t.Fatalf("checkpoint after UIDVALIDITY change = %q, want it cleared", got)
	}
	if _, err := mb.Fetch(context.Background(), 10); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := c.sinceUIDs[len(c.sinceUIDs)-1]; got != 0 {
		t.Fatalf("fetch after renumbering sinceUID = %d, want 0", got)
	}
}
