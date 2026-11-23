package email

import (
	"context"
	"strings"
	"time"

	"windshift/internal/mailbox"
)

// graphMailbox adapts a GraphMailbox to Mailbox. It carries no IMAP concepts:
// there is no UID space and no expunge. Its checkpoint is the delta round's
// resume link, extended with the folder, the baseline cutoff, and the poison id
// so those survive in the existing graph_delta_link column.
type graphMailbox struct {
	box    *GraphMailbox
	folder string
	// cursor is where the next Fetch resumes. committed is where the persisted
	// checkpoint points; the two differ between Fetch and Acknowledge so an
	// early-exit batch is replayed rather than skipped.
	cursor         GraphMailboxCursor
	committed      GraphMailboxCursor
	checkpoint     graphCheckpoint
	holdResume     bool
	folderMismatch bool
	omitPoison     string
}

// NewGraphMailbox wraps a Graph mailbox reader as a Mailbox.
func NewGraphMailbox(box *GraphMailbox) mailbox.Mailbox {
	return &graphMailbox{box: box}
}

func (g *graphMailbox) Open(ctx context.Context, scope string, _ bool) (mailbox.Info, error) {
	g.cursor = GraphMailboxCursor{}
	g.committed = GraphMailboxCursor{}
	g.holdResume = false
	g.omitPoison = ""
	if err := g.box.bindFolder(ctx, scope); err != nil {
		return mailbox.Info{}, err
	}
	g.folder = g.box.folder
	// The restored checkpoint may belong to another folder. Resuming it would
	// keep polling the previous folder, so drop it now that the scope is known.
	if g.checkpoint.Folder != "" && g.checkpoint.Folder != g.folder {
		g.folderMismatch = true
		g.checkpoint = graphCheckpoint{Folder: g.folder}
	}
	// Graph resumes from a delta link rather than a position in a mailbox, so
	// there is no "existing history" concept to skip. A new intake opts into
	// history by starting from an empty link, and into a baseline by calling
	// Seed.
	return mailbox.Info{HasExisting: false}, nil
}

func (g *graphMailbox) Fetch(ctx context.Context, batch int) ([]mailbox.Delivery, error) {
	// The baseline cutoff and the exhausted-poison id must reach the delta
	// round: without them a primed mailbox re-ingests skipped history and a
	// poison message is retried forever.
	messages, cursor, err := g.box.PollMessagesSince(ctx, g.cursor, batch, g.checkpoint.NotBefore, g.omitPoison)
	if err != nil {
		return nil, err
	}
	g.cursor = cursor
	deliveries := make([]mailbox.Delivery, 0, len(messages))
	for _, msg := range messages {
		deliveries = append(deliveries, mailbox.Delivery{Ref: msg.Ref, Message: msg})
	}
	return deliveries, nil
}

func (g *graphMailbox) MarkRead(ctx context.Context, ref string) error {
	// A message the delta round already reported as read needs no PATCH; the
	// legacy adapter suppressed this the same way.
	if g.box.readByID[ref] {
		return nil
	}
	return g.box.MarkRead(ctx, ref)
}

func (g *graphMailbox) Delete(ctx context.Context, ref string) error {
	return g.box.Delete(ctx, ref)
}

// Expunge is a no-op: a Graph delete is immediate, so there is no deferred
// expunge phase to drive.
func (g *graphMailbox) Expunge(context.Context) error { return nil }

// Checkpoint renders the persisted value. It advances only when the round is
// acknowledged; while a resume is held (after a poison failure) it keeps the
// previous link so the failed message is retried rather than skipped.
func (g *graphMailbox) Checkpoint() string {
	cp := g.checkpoint
	cp.Folder = g.folder
	cp.URL = g.committed.DeltaLink
	if strings.TrimSpace(cp.URL) == "" {
		return ""
	}
	return encodeGraphCheckpoint(cp)
}

func (g *graphMailbox) Restore(checkpoint string) {
	cp := parseGraphCheckpoint(checkpoint)
	g.folderMismatch = cp.Folder != "" && g.folder != "" && cp.Folder != g.folder
	g.holdResume = false
	g.omitPoison = ""
	if g.folderMismatch {
		g.checkpoint = graphCheckpoint{Folder: g.folder}
		g.cursor = GraphMailboxCursor{}
		g.committed = GraphMailboxCursor{}
		return
	}
	if cp.Folder == "" {
		cp.Folder = g.folder
	}
	g.checkpoint = cp
	g.cursor = GraphMailboxCursor{DeltaLink: cp.URL}
	g.committed = GraphMailboxCursor{DeltaLink: cp.URL}
}

func (g *graphMailbox) Acknowledge() {
	g.holdResume = false
	g.committed = g.cursor
	g.checkpoint.URL = g.cursor.DeltaLink
	g.checkpoint.PoisonID = ""
	g.checkpoint.PoisonCount = 0
	g.omitPoison = ""
}

func (g *graphMailbox) Close() error { return nil }

// Seed advances one baseline page without downloading MIME. done reports that
// the folder's current state has been acknowledged.
func (g *graphMailbox) Seed(ctx context.Context, batch int) (bool, error) {
	if g.checkpoint.NotBefore.IsZero() {
		g.checkpoint.NotBefore = time.Now().UTC()
	}
	cursor, done, err := g.box.baselinePage(ctx, g.cursor, batch)
	if err != nil {
		return false, err
	}
	g.cursor = cursor
	g.committed = cursor
	g.checkpoint.URL = cursor.DeltaLink
	g.checkpoint.Folder = g.folder
	g.holdResume = false
	return done, nil
}

// FolderMismatch reports that the persisted checkpoint belongs to another
// folder, so the poller must reset its stored cursor.
func (g *graphMailbox) FolderMismatch() bool { return g.folderMismatch }

func (g *graphMailbox) Poison() (string, int, bool) {
	if g.checkpoint.PoisonID == "" {
		return "", 0, false
	}
	return g.checkpoint.PoisonID, g.checkpoint.PoisonCount, true
}

func (g *graphMailbox) SetPoison(ref string, count int) {
	g.checkpoint.PoisonID = ref
	g.checkpoint.PoisonCount = count
	g.holdResume = true
}

func (g *graphMailbox) OmitExhausted(max int) {
	if max > 0 && g.checkpoint.PoisonCount >= max && g.checkpoint.PoisonID != "" {
		g.omitPoison = g.checkpoint.PoisonID
	}
}

// FetchOmit keeps the exhausted poison id out of the next delta round.
func (g *graphMailbox) FetchOmit() string { return g.omitPoison }
