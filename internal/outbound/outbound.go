// Package outbound defines the neutral contract between Windshift's email
// pipeline and a concrete outbound provider (SMTP today, Microsoft Graph
// tomorrow, whatever comes next). Neither side of the contract imports the
// other: providers depend on this package, callers depend on this package,
// and providers never reference each other.
package outbound

import (
	"context"
	"errors"

	"windshift/internal/models"
)

// ThreadedEmailParams carries everything a provider needs to send a threaded
// (In-Reply-To / References) message. Callers render subject/bodies before
// invoking SendThreadedEmail so the subject can be kept stable across the
// thread.
type ThreadedEmailParams struct {
	ToEmail      string
	ToName       string
	ReplyToEmail string
	ReplyToName  string
	Subject      string
	HTMLBody     string
	TextBody     string
	MessageID    string
	InReplyTo    string
	References   []string
}

// Sender is the contract every outbound provider implements. It intentionally
// mirrors the shape the rest of the app already consumes for transactional
// and batched mail.
type Sender interface {
	// IsConfigured reports whether the provider can send (credentials present
	// and usable). It must not perform network I/O.
	IsConfigured() bool

	SendTransactional(toEmail, locale, templateName string, data any) error
	SendTransactionalContext(ctx context.Context, toEmail, locale, templateName string, data any) error
	SendThreadedEmail(params ThreadedEmailParams) error
	SendThreadedEmailContext(ctx context.Context, params ThreadedEmailParams) error

	// RenderEmail renders a named template (admin-edited DB row preferred,
	// embedded fallback otherwise) in the default locale. Returned subject is
	// also exposed to the body as {{.Subject}}.
	RenderEmail(templateName string, data any) (subject, htmlBody, textBody string, err error)
	// RenderEmailLocale is RenderEmail with copy/date bound to a recipient locale.
	RenderEmailLocale(locale, templateName string, data any) (subject, htmlBody, textBody string, err error)
	SendBatchedNotifications(userEmail, userName, locale, timezone string, notifications []models.Notification) error
	SendBatchedNotificationsContext(ctx context.Context, userEmail, userName, locale, timezone string, notifications []models.Notification) error
}

// Encryptor mirrors the at-rest secret cipher used for tenant secrets.
// Providers accept it so configuration secrets can be decrypted at send time.
type Encryptor interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(ciphertext string) (string, error)
}

// ErrNotConfigured is returned when no outbound mail channel can send.
// Providers return this value directly (not wrapped) so equality and
// errors.Is both match. It is decided without network I/O.
var ErrNotConfigured = errors.New("outbound email is not configured")
