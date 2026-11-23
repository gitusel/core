// Package mailmsg declares the transport-neutral message a mailbox hands to
// the parser.
//
// It exists so a Microsoft Graph, Teams, or future file mailbox can be consumed
// without dragging in go-imap: nothing here knows about UIDs, UIDVALIDITY,
// envelopes, or flags. IMAP's *Client adapts its richer fetch result into this
// shape at the package boundary.
package mailmsg

// Message is one fetched message, identified by the reference its transport
// uses to address it later. Raw is the full RFC 5322 payload; callers parse it
// once rather than stitching headers and body back together.
type Message struct {
	// Ref is transport-stable: an IMAP UID rendered as text, a Graph message id,
	// a Teams message id. It must survive across polls so it can be used for
	// de-duplication, acknowledgement, and poison tracking.
	Ref string
	// Raw holds the complete message bytes. Parsing recovers subject, addresses,
	// threading headers, and body from it, so no transport-specific envelope is
	// required.
	Raw []byte
	// FetchError reports a message that was listed by the transport but whose
	// body could not be retrieved. The delivery is still reported so the caller
	// can advance past it or park it as poison rather than silently skipping.
	FetchError error
}
