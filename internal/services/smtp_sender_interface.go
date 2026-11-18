package services

import (
	"context"
	"errors"

	"windshift/internal/outbound"
)

// ErrRecipientIsAgent is returned by transactional senders when the
// recipient User row is flagged is_agent (owned agent or admin-provisioned
// service user). Agents do not consume invitation, verification, or other
// human-facing mail; handlers that race against this should treat the
// error as a soft skip, not a delivery failure.
var ErrRecipientIsAgent = errors.New("recipient is an agent or service user; refusing to send")

// TransactionalEmailSender is the surface needed by services that send
// admin-templated transactional emails (magic-link, email-verification,
// invitation). The concrete sender selected at server startup (SMTP or
// Microsoft Graph) satisfies this interface; tests can substitute a fake.
type TransactionalEmailSender interface {
	IsConfigured() bool
	SendTransactional(toEmail, locale, templateName string, data any) error
}

// MentionEmailSender renders and sends the dedicated @mention email. It is a
// narrower surface than ThreadedEmailSender: mentions are fresh, unthreaded
// messages to internal users, so the sender only needs a context-aware
// transactional send of a named template.
type MentionEmailSender interface {
	IsConfigured() bool
	SendTransactionalContext(ctx context.Context, toEmail, locale, templateName string, data any) error
}

// ThreadedEmailSender covers threaded replies. RenderEmail resolves a named
// template (DB row preferred, embedded fallback otherwise) and renders it
// against the provided data — kept separate from TransactionalEmailSender
// because threaded replies need the rendered subject pre-computed by the
// caller (to keep the "Re: ..." header stable across the email-tracking thread).
type ThreadedEmailSender interface {
	IsConfigured() bool
	SendThreadedEmail(params outbound.ThreadedEmailParams) error
	SendThreadedEmailContext(ctx context.Context, params outbound.ThreadedEmailParams) error
	RenderEmail(templateName string, data any) (subject, htmlBody, textBody string, err error)
	RenderEmailLocale(locale, templateName string, data any) (subject, htmlBody, textBody string, err error)
}
