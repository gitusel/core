package email

import (
	"context"
	"fmt"
	"strconv"

	"windshift/internal/mailmsg"
	"windshift/internal/models"
)

// Delivery is one message handed to the intake poller, tagged with the
// reference its transport uses to address it later. IMAP refs are the decimal
// UID; a Graph ref is the immutable Graph message id. The ref is stable for the
// life of the mailbox, unlike the per-poll ordinals the IMAP-shaped Graph
// adapter still invents for the legacy Poll path.
type Delivery struct {
	Ref     string
	Message *mailmsg.Message
}

// MailboxInfo describes what Open found.
type MailboxInfo struct {
	// HasExisting reports that the mailbox already holds messages, so a new
	// intake must choose between importing history and skipping to the end.
	HasExisting bool
}

// Mailbox is the transport-neutral surface an intake poller drives. It is
// deliberately not IMAP: there is no UID space, UIDVALIDITY, or UIDNEXT here,
// because a Graph or Teams mailbox has none of those. Each transport keeps its
// cursor semantics behind Checkpoint/Restore.
//
// A transport that can skip a mailbox's existing history implements
// MailboxSeeder; one with a poison tracker implements MailboxPoisoner. Both are
// Graph-shaped recovery concerns and stay optional, so an IMAP mailbox does not
// have to pretend to support them.
type Mailbox interface {
	Open(ctx context.Context, scope string) (MailboxInfo, error)
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

// MailboxSeeder is implemented by transports that advance past existing history
// without downloading it. One call acknowledges at most one page; done reports
// that there is nothing left to prime.
type MailboxSeeder interface {
	Seed(ctx context.Context, batch int) (done bool, err error)
}

// MailboxPoisoner tracks a message that repeatedly fails to fetch or process.
// It is Graph-only: an IMAP mailbox pins the failure on failed_message_uid.
type MailboxPoisoner interface {
	Poison() (ref string, count int, ok bool)
	SetPoison(ref string, count int)
	OmitExhausted(max int)
}

// MailboxProvider is optionally implemented by providers that build a native
// Mailbox. Providers that do not are wrapped from their IMAPClient.
type MailboxProvider interface {
	OpenMailbox(ctx context.Context, config *models.ChannelConfig) (Mailbox, error)
}

// imapMailbox adapts an IMAP client to Mailbox. UIDs, UIDVALIDITY, and UIDNEXT
// stay inside this type: the checkpoint is the highest consumed UID and the
// server's validity, encoded so the poller can treat it as opaque.
type imapMailbox struct {
	client IMAPClient
	scope  string
	// nextUID is where the next Fetch starts. committedUID is where the
	// persisted checkpoint points. They differ between Fetch and Acknowledge:
	// a poll that stops early must not persist a cursor past messages it never
	// processed.
	nextUID      uint32
	committedUID uint32
	validity     uint32
	hasHistory   bool
	closed       bool
}

// NewIMAPMailbox wraps an IMAP client as a Mailbox. It does not connect; the
// caller supplies a connected client, exactly as the poller did before.
func NewIMAPMailbox(client IMAPClient) Mailbox {
	return &imapMailbox{client: client}
}

func (m *imapMailbox) Open(ctx context.Context, scope string) (MailboxInfo, error) {
	if err := ctx.Err(); err != nil {
		return MailboxInfo{}, err
	}
	selectData, err := m.client.SelectMailbox(scope)
	if err != nil {
		return MailboxInfo{}, err
	}
	m.scope = scope
	if selectData != nil {
		// A changed UIDVALIDITY means the server renumbered the mailbox, so a
		// cursor from the old UID space would skip or replay mail.
		if m.validity != 0 && selectData.UIDValidity != 0 && selectData.UIDValidity != m.validity {
			m.nextUID = 0
			m.committedUID = 0
		}
		m.validity = selectData.UIDValidity
		m.hasHistory = selectData.UIDNext > 1
	}
	return MailboxInfo{HasExisting: m.hasHistory}, nil
}

func (m *imapMailbox) Fetch(ctx context.Context, batch int) ([]Delivery, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	messages, err := m.client.FetchMessages(m.nextUID, batch)
	if err != nil {
		return nil, err
	}
	deliveries := make([]Delivery, 0, len(messages))
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		deliveries = append(deliveries, Delivery{
			Ref:     strconv.FormatUint(uint64(msg.UID), 10),
			Message: &mailmsg.Message{Ref: strconv.FormatUint(uint64(msg.UID), 10), Raw: msg.Raw, FetchError: msg.FetchError},
		})
		if msg.UID > m.nextUID {
			m.nextUID = msg.UID
		}
	}
	return deliveries, nil
}

func (m *imapMailbox) uid(ref string) (uint32, error) {
	uid, err := strconv.ParseUint(ref, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("imap mailbox ref %q is not a UID: %w", ref, err)
	}
	return uint32(uid), nil
}

func (m *imapMailbox) MarkRead(ctx context.Context, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	uid, err := m.uid(ref)
	if err != nil {
		return err
	}
	return m.client.MarkAsRead(uid)
}

func (m *imapMailbox) Delete(ctx context.Context, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	uid, err := m.uid(ref)
	if err != nil {
		return err
	}
	return m.client.DeleteMessage(uid)
}

func (m *imapMailbox) Expunge(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.client.Expunge()
}

// Checkpoint encodes the last acknowledged UID together with the server's
// UIDVALIDITY, so a UID space reset is detectable on the next tick. It stays
// empty until a batch is acknowledged: nothing is persisted for messages the
// poller has only looked at.
func (m *imapMailbox) Checkpoint() string {
	if m.committedUID == 0 {
		return ""
	}
	return fmt.Sprintf("imap-v1:%d:%d", m.validity, m.committedUID)
}

func (m *imapMailbox) Restore(checkpoint string) {
	validity, lastUID, ok := parseIMAPCheckpoint(checkpoint)
	if !ok {
		return
	}
	m.validity, m.committedUID = validity, lastUID
	m.nextUID = lastUID
}

// Acknowledge commits the batch: the fetch position becomes the persisted
// cursor. It is not a no-op even for IMAP, because a batch can stop early.
func (m *imapMailbox) Acknowledge() { m.committedUID = m.nextUID }

func (m *imapMailbox) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	return m.client.Close()
}

func parseIMAPCheckpoint(raw string) (uint32, uint32, bool) {
	var validity, lastUID uint32
	if _, err := fmt.Sscanf(raw, "imap-v1:%d:%d", &validity, &lastUID); err != nil {
		return 0, 0, false
	}
	return validity, lastUID, true
}
