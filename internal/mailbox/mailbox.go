// Package mailbox declares the transport-neutral contract between an intake
// poller and whatever holds the messages: an IMAP mailbox today, a Graph
// mailbox next to it, a Teams channel later.
//
// Nothing here mentions IMAP or SMTP. A transport keeps its own addressing and
// cursor semantics behind an opaque checkpoint string, so adding a new one does
// not require teaching the poller a new protocol.
package mailbox

import (
	"context"

	"windshift/internal/mailmsg"
	"windshift/internal/models"
)

// Delivery is one message handed to the poller, tagged with the reference its
// transport uses to address it later. An IMAP ref is the decimal UID; a Graph
// ref is the immutable Graph message id. The ref is stable for the life of the
// mailbox, unlike a per-poll ordinal.
type Delivery struct {
	Ref     string
	Message *mailmsg.Message
}

// Info describes what Open found.
type Info struct {
	// HasExisting reports that the mailbox already holds messages, so a new
	// intake must choose between importing history and skipping to the end.
	HasExisting bool
	// Validity identifies the transport's current cursor generation. For IMAP it
	// is the server's UIDVALIDITY, and a change means the mailbox was renumbered,
	// so previous watermarks and dedup keys no longer apply. A transport without
	// generations reports 0.
	Validity uint32
}

// Mailbox is the surface an intake poller drives.
type Mailbox interface {
	// Open selects the folder or channel. skipExisting starts a fresh intake at
	// the current end of the mailbox instead of importing its history.
	Open(ctx context.Context, scope string, skipExisting bool) (Info, error)
	Fetch(ctx context.Context, batch int) ([]Delivery, error)
	MarkRead(ctx context.Context, ref string) error
	Delete(ctx context.Context, ref string) error
	Expunge(ctx context.Context) error
	// Checkpoint returns the opaque value to persist for the next tick. It
	// advances only after Acknowledge, so a batch that stops early resumes where
	// it stopped instead of skipping the messages it never processed.
	Checkpoint() string
	Restore(checkpoint string)
	Acknowledge()
	Close() error
}

// Seeder is implemented by transports that advance past existing history
// without downloading it. One call acknowledges at most one page; done reports
// that there is nothing left to prime.
type Seeder interface {
	Seed(ctx context.Context, batch int) (done bool, err error)
}

// Poisoner tracks a message that repeatedly fails to fetch or process. It is a
// Graph-shaped concern; an IMAP mailbox pins the failure on failed_message_uid.
type Poisoner interface {
	Poison() (ref string, count int, ok bool)
	SetPoison(ref string, count int)
	OmitExhausted(max int)
}

// Provider is optionally implemented by a provider that builds a native
// Mailbox. A provider that does not is wrapped from its IMAP client.
type Provider interface {
	OpenMailbox(ctx context.Context, config *models.ChannelConfig) (Mailbox, error)
}
