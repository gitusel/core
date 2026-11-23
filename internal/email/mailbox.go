package email

import (
	"context"
	"fmt"
	"strconv"

	"windshift/internal/mailbox"
	"windshift/internal/mailmsg"
)

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
func NewIMAPMailbox(client IMAPClient) mailbox.Mailbox {
	return &imapMailbox{client: client}
}

func (m *imapMailbox) Open(ctx context.Context, scope string, skipExisting bool) (mailbox.Info, error) {
	if err := ctx.Err(); err != nil {
		return mailbox.Info{}, err
	}
	selectData, err := m.client.SelectMailbox(scope)
	if err != nil {
		return mailbox.Info{}, err
	}
	if scope != m.scope {
		// One mailbox drives several intakes. A cursor from the previous folder
		// must not leak into this one.
		m.nextUID = 0
		m.committedUID = 0
	}
	m.scope = scope
	if selectData == nil {
		return mailbox.Info{}, nil
	}
	// A changed UIDVALIDITY means the server renumbered the mailbox, so a
	// cursor and dedup keys from the old UID space would skip or replay mail.
	if m.validity != 0 && selectData.UIDValidity != 0 && selectData.UIDValidity != m.validity {
		m.nextUID = 0
		m.committedUID = 0
	}
	m.validity = selectData.UIDValidity
	m.hasHistory = selectData.UIDNext > 1
	if skipExisting && selectData.UIDNext > 1 {
		// Start after the newest existing message. The old poller expressed this
		// as sinceUID = UIDNext-1; keeping it inside the mailbox means the
		// scheduler never has to reason about UID arithmetic. The position is
		// committed immediately: skipping history is itself an acknowledged
		// decision, and a crash before the first message must not re-import.
		m.nextUID = uint32(selectData.UIDNext - 1)
		m.committedUID = m.nextUID
	}
	return mailbox.Info{HasExisting: m.hasHistory, Validity: m.validity}, nil
}

func (m *imapMailbox) Fetch(ctx context.Context, batch int) ([]mailbox.Delivery, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	messages, err := m.client.FetchMessages(m.nextUID, batch)
	if err != nil {
		return nil, err
	}
	deliveries := make([]mailbox.Delivery, 0, len(messages))
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		deliveries = append(deliveries, mailbox.Delivery{
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

// CommittedUID reports the last acknowledged IMAP UID so a caller that still
// stores numeric watermarks can keep them in step with the opaque checkpoint.
// It returns 0 for a legacy Graph-compatible client, which has no UID space.
func (m *imapMailbox) CommittedUID() uint32 { return m.committedUID }

func (m *imapMailbox) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	return m.client.Close()
}

// EncodeIMAPCheckpoint renders an IMAP cursor so a caller that stores numeric
// watermarks (or one migrating from them) can feed it back through
// Mailbox.Restore unchanged.
func EncodeIMAPCheckpoint(validity uint32, lastUID uint32) string {
	if lastUID == 0 {
		return ""
	}
	return fmt.Sprintf("imap-v1:%d:%d", validity, lastUID)
}

func parseIMAPCheckpoint(raw string) (uint32, uint32, bool) {
	var validity, lastUID uint32
	if _, err := fmt.Sscanf(raw, "imap-v1:%d:%d", &validity, &lastUID); err != nil {
		return 0, 0, false
	}
	return validity, lastUID, true
}
