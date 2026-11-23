// Package teams declares the transport-neutral contract for reading a Microsoft
// Teams channel, plus the capability a provider implements to offer one.
//
// This is deliberately not a mailbox. A Teams channel has no folders, no UID
// space, and no per-message mark-read or delete: conversations are addressed by
// team and channel id, resumed from a lastModifiedDateTime watermark, and
// changed only by posting a reply. Forcing a channel into the Mailbox contract
// would mean inventing folder and read-state semantics that Teams rejects.
//
// The message type is shared with mail because a Graph chatMessage is not an
// RFC 5322 message, but a consumer only needs a stable ref, a body, and an
// author — see Message. Teams posts are HTML fragments, not MIME.
package teams

import (
	"context"
	"errors"

	"windshift/internal/models"
)

var (
	// ErrNotAvailable reports that a provider can reach Teams in principle but
	// this build does not implement it yet. It is distinct from a configuration
	// or credential error so an operator sees "not implemented", not
	// "misconfigured".
	ErrNotAvailable = errors.New("teams workload is not implemented yet")
	// ErrMissingConfig reports that the provider cannot open the channel with the
	// configuration it was given.
	ErrMissingConfig = errors.New("teams channel configuration is incomplete")
)

// ChannelRef identifies one channel. Teams addresses messages by team and
// channel id; there is no folder to select and no stable ordinal.
type ChannelRef struct {
	TeamID    string
	ChannelID string
}

// Message is one channel message as delivered to a consumer.
type Message struct {
	// Ref is the Graph chatMessage id. It is stable for the life of the message
	// and is the only handle a caller should persist.
	Ref string
	// BodyHTML is the message body as Graph returns it. Teams sends HTML
	// fragments, so this is not an RFC 5322 payload.
	BodyHTML string
	// Author is the display name Graph reports, or the application name for a
	// bot post. May be empty for a system message.
	Author string
	// CreatedAt is Graph's createdDateTime in RFC 3339 form, empty when absent.
	CreatedAt string
	// ReplyToID is set for a reply and empty for a root post.
	ReplyToID string
}

// Batch is one page of channel messages plus the checkpoint that resumes after
// it. The checkpoint is opaque: callers persist it and hand it back unchanged.
type Batch struct {
	Messages   []Message
	Checkpoint string
}

// Source reads one Teams channel. It is a capability in its own right, not a
// specialization of a mailbox.
type Source interface {
	// Open validates that the channel exists and is reachable with the
	// configured credentials. It performs at most one Graph call.
	Open(ctx context.Context, ref ChannelRef) error
	// Fetch returns the messages created or changed after checkpoint, oldest
	// first. An empty checkpoint starts from the channel's current state.
	Fetch(ctx context.Context, checkpoint string, batch int) (Batch, error)
	// Post replies into the channel. replyToID empty starts a new root post.
	Post(ctx context.Context, replyToID, bodyHTML string) (Message, error)
	Close() error
}

// Provider is implemented by a provider that can open a Teams channel. It is
// one of several Microsoft Graph capabilities a provider may offer, alongside
// the mailbox capability for mail and a later one for SharePoint files; none of
// them implies another.
type Provider interface {
	OpenChannel(ctx context.Context, config *models.ChannelConfig, ref ChannelRef) (Source, error)
}
