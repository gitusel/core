// Package smtp provides SMTP email sending functionality for notifications,
// including support for batched notification delivery and various encryption methods.
package smtp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"sync"
	"time"

	"windshift/internal/database"
	"windshift/internal/emailutil"
	"windshift/internal/i18n"
	"windshift/internal/models"
	"windshift/internal/operatorcontrols"
	"windshift/internal/outbound"
	"windshift/internal/repository"
	"windshift/internal/utils"
)

// EncryptionModeAllowed reports whether the sender can dispatch with the
// configured mode. Unknown and empty modes are rejected so a typo can never
// silently downgrade a connection to plaintext.
func EncryptionModeAllowed(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "tls", "starttls", "ssl", "none":
		return true
	default:
		return false
	}
}

// ValidateTransport rejects unsupported modes and authentication without TLS.
// Plaintext SMTP is intended for trusted relays that authorize by network;
// credentials must never be exposed on the connection.
func ValidateTransport(config *models.ChannelConfig) error {
	if config == nil {
		return fmt.Errorf("SMTP config is required")
	}
	mode := strings.ToLower(strings.TrimSpace(config.SMTPEncryption))
	if !EncryptionModeAllowed(mode) {
		return fmt.Errorf("SMTP encryption %q not allowed; use \"tls\", \"starttls\", \"ssl\", or \"none\"", config.SMTPEncryption)
	}
	if mode == "none" && (strings.TrimSpace(config.SMTPUsername) != "" || config.SMTPPassword != "") {
		return ErrSMTPAuthenticationRequiresTLS
	}
	if !AuthMethodAllowed(config.SMTPAuthMethod) {
		return fmt.Errorf("SMTP auth method %q not allowed; use \"auto\", \"plain\", \"login\", \"cram-md5\", or \"none\"", config.SMTPAuthMethod)
	}
	return nil
}

// ErrSMTPAuthenticationRequiresTLS is returned when a plaintext channel
// contains credentials that would otherwise be exposed on the connection.
var ErrSMTPAuthenticationRequiresTLS = errors.New("SMTP authentication requires TLS")

// Encryptor mirrors email.Encryptor — duplicated here to avoid an
// smtp→email→services→smtp import cycle. *sso.SecretEncryption (the same
// concrete type passed to email.NewCredentialManager) satisfies both
// interfaces, so production wiring uses one instance.
type Encryptor interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(ciphertext string) (string, error)
}

const encryptedSecretPrefix = "enc:v1:"

// decryptOrLegacy mirrors email/credentials.go::DecryptOrLegacy. The legacy-
// plaintext fallback (return value verbatim if it's not a valid AES-GCM
// ciphertext) lets pre-encryption rows keep working through a rolling
// migration of SMTPPassword from plaintext to AES-GCM. AES-GCM minimum is
// 12-byte nonce + 16-byte tag = 28 raw bytes, ~40 base64 chars.
func decryptOrLegacy(enc Encryptor, value string) (string, error) {
	if value == "" || enc == nil {
		return value, nil
	}
	if strings.HasPrefix(value, encryptedSecretPrefix) {
		plain, err := enc.Decrypt(strings.TrimPrefix(value, encryptedSecretPrefix))
		if err != nil {
			return "", fmt.Errorf("decrypt SMTP secret: %w", err)
		}
		return plain, nil
	}
	const minCipherBytes = 28
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(raw) < minCipherBytes {
		return value, nil //nolint:nilerr // legacy-plaintext fallback is intentional
	}
	plain, err := enc.Decrypt(value)
	if err != nil {
		return value, nil //nolint:nilerr // legacy plaintext may itself be long base64
	}
	return plain, nil
}

// smtpDialTimeout caps how long we'll wait to establish a TCP/TLS connection
// to an SMTP server. Without this the scheduler can hang its 5-minute tick
// on a single wedged MX, stalling notifications for every other user.
const smtpDialTimeout = 15 * time.Second
const smtpOperationTimeout = 30 * time.Second

// A global SMTP outage must not make every outbound path wait out its own
// per-message timeout. After this many consecutive connection-level failures
// the sender stops dialing for the cooldown and callers release or retry their
// claims without a network attempt.
const (
	smtpBreakerFailureThreshold = 3
	smtpBreakerCooldown         = time.Minute
)

// ErrSMTPCircuitOpen is returned while the shared SMTP breaker is open.
var ErrSMTPCircuitOpen = errors.New("SMTP circuit breaker is open")

// hostFromAddr returns just the host portion of an SMTP address, handling
// IPv6 bracketed notation safely. The old `strings.Split(addr, ":")[0]` path
// returned "[" for `[::1]:465`, which makes TLS SNI verification impossible.
func hostFromAddr(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// sanitizeHeader strips CR/LF from a string intended for an SMTP header value.
// Values that land here include admin-configured from-names and fully
// user-controlled content (e.g., inbound Subject or Message-ID re-emitted on
// a reply), so a single `\r\n` in the input lets an attacker inject arbitrary
// Bcc/Cc/Reply-To headers downstream.
func sanitizeHeader(s string) string {
	if s == "" {
		return s
	}
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// encodeHeaderWord returns a value safe to use as an unstructured header
// (Subject, From display name, To display name). It strips CR/LF and
// RFC 2047-encodes anything non-ASCII so foreign-language subjects don't
// wire-break the message.
func encodeHeaderWord(s string) string {
	s = sanitizeHeader(s)
	if s == "" {
		return s
	}
	return mime.QEncoding.Encode("utf-8", s)
}

// encodeMailbox formats a "Name <addr>" mailbox header value safely. The
// address is sanitized but not QEncoded (it has to stay RFC 5322 addr-spec).
//
// The display name goes through net/mail, which quotes the RFC 5322 phrase or
// emits an encoded-word as needed. An unquoted name containing specials (",",
// ";", "<", "@", ...) turns the header into an address list, so a portal
// customer named "Lastname, Firstname" made receivers split the To header into
// a broken "Lastname" mailbox and the real address.
func encodeMailbox(name, addr string) string {
	addr = sanitizeHeader(addr)
	name = sanitizeHeader(name)
	if name == "" {
		return addr
	}
	address := &mail.Address{Name: name, Address: addr}
	if address.Address == "" {
		return addr
	}
	return address.String()
}

// normalizeEnvelopeAddress accepts only a bare addr-spec for SMTP MAIL/RCPT
// commands. Header sanitization alone is insufficient at this layer: display
// names, address lists, or control characters must never reach net/smtp's
// command formatter.
func normalizeEnvelopeAddress(raw string) (string, error) {
	address := strings.TrimSpace(raw)
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address == "" || parsed.Address != address {
		return "", fmt.Errorf("invalid bare email address")
	}
	return address, nil
}

// NotificationSMTPSender handles sending batched notifications via email.
// Encryptor is optional: when non-nil, getSMTPConfig decrypts SMTPPassword
// before handing the config to dispatch(). If unset (e.g. in tests), the
// password is passed through verbatim — matching the legacy-plaintext
// fallback in decryptOrLegacy.
type NotificationSMTPSender struct {
	db         database.Database
	templates  *repository.EmailTemplateRepository
	encryption Encryptor
	baseURL    string

	breakerMu        sync.Mutex
	breakerFailures  int
	breakerOpenUntil time.Time

	// Idle authenticated sessions are reused across consecutive sends so a drain
	// pass or digest tick does not pay a TCP + TLS + AUTH handshake per message.
	// A small pool keeps concurrent transactional sends from queueing behind a
	// long digest delivery.
	sessionMu sync.Mutex
	sessions  []*smtpSession
}

const (
	smtpSessionIdleTimeout = 30 * time.Second
	smtpSessionPoolSize    = 4
)

// smtpSession is a reusable authenticated SMTP connection.
type smtpSession struct {
	conn        net.Conn
	client      *smtp.Client
	fingerprint string
	lastUsed    time.Time
}

// NewNotificationSMTPSender creates a new SMTP notification sender
func NewNotificationSMTPSender(db database.Database) *NotificationSMTPSender {
	return &NotificationSMTPSender{
		db:        db,
		templates: repository.NewEmailTemplateRepository(db),
	}
}

// SetBaseURL sets the browser-visible base URL used to turn notification
// action paths into clickable links in digest emails. Called from server
// startup.
func (s *NotificationSMTPSender) SetBaseURL(baseURL string) {
	s.baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

// SetEncryption wires the at-rest encryption service used to decrypt
// SMTPPassword before SMTP AUTH. Called from server startup with the same
// *sso.SecretEncryption instance that the email/SCM/integration handlers use.
func (s *NotificationSMTPSender) SetEncryption(enc Encryptor) {
	s.encryption = enc
}

// RenderEmail resolves the named email template (admin-edited DB row preferred,
// embedded fallback otherwise) and renders subject/html/text against `data` in
// the default locale. The rendered Subject is also exposed to the HTML/text body
// templates as `{{.Subject}}` (used by the shared shell's <title> tag), so each
// call site only has to pass its native fields without remembering to mirror
// the subject in the struct.
func (s *NotificationSMTPSender) RenderEmail(templateName string, data any) (subject, htmlBody, textBody string, err error) {
	return s.RenderEmailLocale(i18n.Default, templateName, data)
}

// RenderEmailLocale is RenderEmail with copy and date formatting bound to the
// recipient's locale.
func (s *NotificationSMTPSender) RenderEmailLocale(locale, templateName string, data any) (subject, htmlBody, textBody string, err error) {
	return emailutil.RenderNamed(locale, templateName, func(loc, name string) (emailutil.TemplateSource, error) {
		subjectSrc, htmlSrc, textSrc, fromDB := s.resolveTemplate(loc, name)
		return emailutil.TemplateSource{Subject: subjectSrc, HTMLBody: htmlSrc, TextBody: textSrc, FromDB: fromDB}, nil
	}, data)
}

// SendTransactional renders a named template against `data` and sends it via
// SMTP to `toEmail` in the recipient's locale. Returns outbound.ErrNotConfigured if
// SMTP isn't set up. This is the shared one-call surface used by the magic-link
// / verification / invitation services so each one stays a thin URL-builder +
// caller.
func (s *NotificationSMTPSender) SendTransactional(toEmail, locale, templateName string, data any) error {
	return s.SendTransactionalContext(context.Background(), toEmail, locale, templateName, data)
}

// SendTransactionalContext is SendTransactional with cancellation propagated
// through SMTP dialing, handshake, commands, and DATA completion. The
// mention-email outbox uses it so a scheduler shutdown cannot wedge a send.
func (s *NotificationSMTPSender) SendTransactionalContext(ctx context.Context, toEmail, locale, templateName string, data any) error {
	if !s.IsConfigured() {
		return outbound.ErrNotConfigured
	}
	subject, htmlBody, textBody, err := s.RenderEmailLocale(locale, templateName, data)
	if err != nil {
		return fmt.Errorf("render %s: %w", templateName, err)
	}
	config, err := s.getSMTPConfig()
	if err != nil {
		return fmt.Errorf("failed to get SMTP config: %w", err)
	}
	return s.sendEmailContext(ctx, config, toEmail, subject, htmlBody, textBody)
}

// resolveTemplate returns the template sources and whether they came from the
// admin-editable DB row (as opposed to the embedded default). It resolves the
// recipient locale first: an exact-locale override wins, then a customized base
// row; an unmodified seed falls through to the embedded default, which is
// localized at render time.
func (s *NotificationSMTPSender) resolveTemplate(locale, name string) (subject, html, text string, fromDB bool) {
	if s.templates != nil {
		if t, fromDB, err := s.templates.ResolveForLocale(name, locale); err == nil && t != nil {
			return t.Subject, t.HTMLBody, t.TextBody, fromDB
		}
	}
	if t, ok := emailutil.LookupDefaultTemplate(name); ok {
		return t.Subject, t.HTMLBody, t.TextBody, false
	}
	return "", "", "", false
}

// IsConfigured checks if SMTP is properly configured
func (s *NotificationSMTPSender) IsConfigured() bool {
	config, err := s.getSMTPConfig()
	if err != nil {
		return false
	}

	// Check that essential SMTP fields are configured
	return config.SMTPHost != "" &&
		config.SMTPPort > 0 &&
		config.SMTPFromEmail != ""
}

// getSMTPConfig retrieves the active SMTP configuration
func (s *NotificationSMTPSender) getSMTPConfig() (*models.ChannelConfig, error) {
	controls := operatorcontrols.From(s.db)
	if err := controls.Check(operatorcontrols.MailSend); err != nil {
		return nil, err
	}
	if managed := controls.Mail(); managed != nil && managed.Mode == "managed" {
		return managedSMTPConfig(managed), nil
	}
	query := `
		SELECT COALESCE(config, '{}') FROM channels
		WHERE type = 'smtp' AND direction = 'outbound'
		  AND status = 'enabled' AND is_default = true
		ORDER BY updated_at DESC
		LIMIT 1
	`

	var configJSON string
	err := s.db.QueryRow(query).Scan(&configJSON)
	if err != nil {
		return nil, fmt.Errorf("no active SMTP configuration found: %w", err)
	}

	var config models.ChannelConfig
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return nil, fmt.Errorf("failed to parse SMTP configuration: %w", err)
	}

	// SMTPPassword stays encrypted in the returned config — dispatch decrypts
	// it just before SMTP AUTH. Keeping the encrypted value in the struct
	// avoids handing plaintext to any code paths that don't strictly need it.
	return &config, nil
}

// SendBatchedNotifications sends a batch of notifications to a user via email
func (s *NotificationSMTPSender) SendBatchedNotifications(userEmail, userName, locale, timezone string, notifications []models.Notification) error {
	return s.SendBatchedNotificationsContext(context.Background(), userEmail, userName, locale, timezone, notifications)
}

// SendBatchedNotificationsContext sends a batch with cancellation propagated
// through SMTP dialing, handshake, commands, and DATA completion. locale and
// timezone are the recipient's preferences and drive date rendering.
func (s *NotificationSMTPSender) SendBatchedNotificationsContext(ctx context.Context, userEmail, userName, locale, timezone string, notifications []models.Notification) error {
	if len(notifications) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	config, err := s.getSMTPConfig()
	if err != nil {
		return fmt.Errorf("failed to get SMTP config: %w", err)
	}

	subject, htmlBody, textBody, err := s.RenderEmailLocale(locale, emailutil.TemplateNotificationBatch, emailutil.BuildNotificationBatchData(s.baseURL, userName, locale, timezone, notifications))
	if err != nil {
		return fmt.Errorf("failed to render notification email: %w", err)
	}

	return s.sendEmailContext(ctx, config, userEmail, subject, htmlBody, textBody)
}

// mimeOptions captures everything that can vary between transactional and
// threaded MIME bodies. Threading-related fields (MessageID, InReplyTo,
// References) and ToName are optional — when empty they are simply omitted
// from the rendered headers, which keeps transactional sends terse and
// threaded sends RFC 5322-correct without two parallel builders.
type mimeOptions struct {
	FromEmail, FromName  string
	ToEmail, ToName      string
	ReplyToEmail         string
	ReplyToName          string
	Subject              string
	HTMLBody, TextBody   string
	MessageID, InReplyTo string
	References           []string
}

// buildMime renders a multipart/alternative message. The MIME structure comes
// from mime/multipart, so the boundary in Content-Type and the delimiters in
// the body are written by the same writer and cannot disagree. Every header
// value that originates from inbound parsing or admin-configured display names
// goes through sanitizeHeader/encodeHeaderWord/encodeMailbox so a stray `\r\n`
// cannot inject Bcc/Cc/Reply-To downstream.
func buildMime(opts mimeOptions) ([]byte, error) {
	boundary, err := newMimeBoundary()
	if err != nil {
		return nil, err
	}

	var body bytes.Buffer
	parts := multipart.NewWriter(&body)
	// mime.FormatMediaType quotes the boundary when it needs to; the body
	// delimiters use the raw value, so they always match.
	if err := parts.SetBoundary(boundary); err != nil {
		return nil, fmt.Errorf("set MIME boundary: %w", err)
	}
	if err := writeMimePart(parts, "text/plain", opts.TextBody); err != nil {
		return nil, err
	}
	if err := writeMimePart(parts, "text/html", opts.HTMLBody); err != nil {
		return nil, err
	}
	if err := parts.Close(); err != nil {
		return nil, fmt.Errorf("close MIME multipart: %w", err)
	}

	var message bytes.Buffer
	writeMimeHeader(&message, "From", encodeMailbox(opts.FromName, opts.FromEmail))
	to := sanitizeHeader(opts.ToEmail)
	if opts.ToName != "" {
		to = encodeMailbox(opts.ToName, opts.ToEmail)
	}
	writeMimeHeader(&message, "To", to)
	writeMimeHeader(&message, "Subject", encodeHeaderWord(opts.Subject))
	writeMimeHeader(&message, "MIME-Version", "1.0")
	writeMimeHeader(&message, "Content-Type", mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": boundary}))

	// Route customer replies to the monitored inbox rather than the SMTP
	// sender. Omit the header when there is no distinct monitored address, so
	// the transport falls back to From exactly as before.
	if replyTo := strings.TrimSpace(opts.ReplyToEmail); replyTo != "" && !strings.EqualFold(replyTo, opts.FromEmail) {
		writeMimeHeader(&message, "Reply-To", encodeMailbox(opts.ReplyToName, replyTo))
	}
	if messageID := formatMessageIDHeader(opts.MessageID); messageID != "" {
		writeMimeHeader(&message, "Message-ID", messageID)
	}
	if inReplyTo := formatMessageIDHeader(opts.InReplyTo); inReplyTo != "" {
		writeMimeHeader(&message, "In-Reply-To", inReplyTo)
	}
	if len(opts.References) > 0 {
		clean := make([]string, 0, len(opts.References))
		for _, ref := range opts.References {
			if formatted := formatMessageIDHeader(ref); formatted != "" {
				clean = append(clean, formatted)
			}
		}
		if len(clean) > 0 {
			message.WriteString(foldHeaderValue("References", strings.Join(clean, " ")))
		}
	}

	message.WriteString("\r\n")
	message.Write(body.Bytes())
	return message.Bytes(), nil
}

// newMimeBoundary returns a random hex boundary. A hex string is a valid RFC
// 2046 b-char token and has no leading hyphen run, which strict parsers (SES)
// mis-attributed to the Content-Type parameter when the boundary started with
// "----=".
func newMimeBoundary() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate MIME boundary: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// writeMimeHeader emits one header line. Values are already sanitized or
// encoded by the caller, so nothing here can introduce a CRLF.
func writeMimeHeader(b *bytes.Buffer, name, value string) {
	b.WriteString(name)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\r\n")
}

// writeMimePart appends one quoted-printable text part. The encoding keeps
// every line inside the 998-octet SMTP limit and carries non-ASCII bodies
// byte-for-byte, which the previous unconditional 8bit declaration did not.
func writeMimePart(parts *multipart.Writer, mediaType, content string) error {
	// The headers are written through CreatePart rather than by hand: a part
	// whose first header is preceded by an empty line loses its whole header
	// block in the receiving parser.
	header := textproto.MIMEHeader{}
	header["Content-Type"] = []string{mediaType + "; charset=UTF-8"}
	header["Content-Transfer-Encoding"] = []string{"quoted-printable"}
	writer, err := parts.CreatePart(header)
	if err != nil {
		return fmt.Errorf("create %s MIME part: %w", mediaType, err)
	}
	quoted := quotedprintable.NewWriter(writer)
	if _, err := io.WriteString(quoted, content); err != nil {
		return fmt.Errorf("write %s MIME part: %w", mediaType, err)
	}
	if err := quoted.Close(); err != nil {
		return fmt.Errorf("encode %s MIME part: %w", mediaType, err)
	}
	return nil
}

// foldHeaderValue folds a space-separated header value at 78 columns per RFC
// 5322, keeping every line inside the 998-octet SMTP hard limit. Long Message-ID
// chains (References) would otherwise exceed the limit on long threads.
func foldHeaderValue(name, value string) string {
	const softLimit = 78
	var b strings.Builder
	b.WriteString(name)
	b.WriteString(": ")
	column := len(name) + 2
	for i, field := range strings.Fields(value) {
		if i > 0 {
			if column+1+len(field) > softLimit {
				b.WriteString("\r\n ")
				column = 1
			} else {
				b.WriteString(" ")
				column++
			}
		}
		b.WriteString(field)
		column += len(field)
	}
	b.WriteString("\r\n")
	return b.String()
}

// formatMessageIDHeader tolerates historical tracking rows that stored the
// go-imap ENVELOPE form without angle brackets and always emits RFC 5322's
// bracketed form on the wire.
func formatMessageIDHeader(value string) string {
	value = strings.TrimSpace(sanitizeHeader(value))
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "<"), ">"))
	if value == "" || strings.ContainsAny(value, "<> \t") {
		return ""
	}
	return "<" + value + ">"
}

// dispatchContext picks the configured transport and sends the assembled MIME
// message. Shared by sendEmail and SendThreadedEmail so the encryption switch
// lives in exactly one place. Plaintext SMTP is allowed only without
// authentication; empty or unknown modes are errors rather than downgrades.
//
// It is a method (rather than a free function) so it can decrypt the at-rest
// SMTPPassword before passing it to SMTP AUTH — every caller goes through the
// encryption-aware sender, even the channel-test path that loads raw config
// from the DB on its own.
func (s *NotificationSMTPSender) dispatchContext(ctx context.Context, config *models.ChannelConfig, toEmail string, message []byte) error {
	controls := operatorcontrols.From(s.db)
	if err := controls.Check(operatorcontrols.MailSend); err != nil {
		return err
	}
	if managed := controls.Mail(); managed != nil && managed.Mode == "managed" {
		if config == nil || config.SMTPFromEmail != managed.FromAddress {
			return &operatorcontrols.DeniedError{Capability: operatorcontrols.MailTransport}
		}
		config = managedSMTPConfig(managed)
	} else if err := controls.Check(operatorcontrols.GenericMail); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if config == nil {
		return fmt.Errorf("SMTP config is required")
	}
	fromEmail, err := normalizeEnvelopeAddress(config.SMTPFromEmail)
	if err != nil {
		return fmt.Errorf("invalid SMTP from address: %w", err)
	}
	toEmail, err = normalizeEnvelopeAddress(toEmail)
	if err != nil {
		return fmt.Errorf("invalid SMTP recipient address: %w", err)
	}
	if strings.TrimSpace(config.SMTPHost) == "" || config.SMTPPort <= 0 || config.SMTPPort > 65535 {
		return fmt.Errorf("invalid SMTP host or port")
	}
	if err := ValidateTransport(config); err != nil {
		return err
	}
	if err := controls.Consume(ctx, "mail_recipients", 1, controls.Limits().MailRecipientsPerDay, 24*time.Hour); err != nil {
		return err
	}
	password := config.SMTPPassword
	if managed := controls.Mail(); managed == nil || managed.Mode != "managed" {
		password, err = decryptOrLegacy(s.encryption, config.SMTPPassword)
	}
	if err != nil {
		return err
	}

	auth, err := newSMTPAuth(config.SMTPAuthMethod, config.SMTPUsername, password, config.SMTPHost)
	if err != nil {
		return err
	}

	addr := fmt.Sprintf("%s:%d", config.SMTPHost, config.SMTPPort)

	if s.breakerOpen() {
		return ErrSMTPCircuitOpen
	}

	mode := strings.ToLower(strings.TrimSpace(config.SMTPEncryption))
	switch mode {
	case "tls", "starttls", "ssl", "none":
	default:
		return fmt.Errorf("SMTP encryption %q not allowed", config.SMTPEncryption)
	}

	fingerprint := fmt.Sprintf("%s|%d|%s|%s|%s|%s|%t",
		config.SMTPHost, config.SMTPPort, mode, config.SMTPUsername,
		config.SMTPPassword, config.SMTPAuthMethod, config.SMTPSkipTLSVerify)

	session, err := s.acquireSession(ctx, fingerprint, addr, mode, auth, config.SMTPSkipTLSVerify)
	if err != nil {
		s.observeDispatch(err)
		return err
	}
	if err := session.send(ctx, fromEmail, toEmail, message); err != nil {
		s.releaseSession(session, false)
		s.observeDispatch(err)
		return err
	}
	s.releaseSession(session, true)
	s.observeDispatch(nil)
	return nil
}

func managedSMTPConfig(policy *operatorcontrols.MailPolicy) *models.ChannelConfig {
	return &models.ChannelConfig{SMTPHost: policy.Host, SMTPPort: policy.Port, SMTPEncryption: policy.Encryption, SMTPUsername: policy.Username, SMTPPassword: policy.Password, SMTPAuthMethod: "auto", SMTPFromEmail: policy.FromAddress, SMTPFromName: policy.FromName}
}

// acquireSession returns a live session for the config, preferring an idle
// pooled connection. Callers release it when the send finishes.
func (s *NotificationSMTPSender) acquireSession(ctx context.Context, fingerprint, addr, mode string, auth smtp.Auth, skipTLSVerify bool) (*smtpSession, error) {
	for {
		s.sessionMu.Lock()
		var candidate *smtpSession
		kept := s.sessions[:0]
		for _, session := range s.sessions {
			if session.fingerprint != fingerprint {
				_ = session.client.Close()
				_ = session.conn.Close()
				continue
			}
			if candidate == nil {
				candidate = session
				continue
			}
			kept = append(kept, session)
		}
		s.sessions = kept
		s.sessionMu.Unlock()

		if candidate == nil {
			break
		}
		if time.Since(candidate.lastUsed) < smtpSessionIdleTimeout {
			if err := setSMTPDeadline(ctx, candidate.conn); err == nil {
				if err := candidate.client.Noop(); err == nil {
					return candidate, nil
				}
			}
		}
		_ = candidate.client.Close()
		_ = candidate.conn.Close()
	}

	conn, client, err := openSMTPClient(ctx, addr, mode, skipTLSVerify)
	if err != nil {
		return nil, err
	}
	if auth != nil {
		stopAuthCancellation := context.AfterFunc(ctx, func() { _ = conn.Close() })
		err := client.Auth(auth)
		stopAuthCancellation()
		if err != nil { //nolint:gocritic
			_ = client.Close()
			_ = conn.Close()
			return nil, err
		}
	}
	return &smtpSession{conn: conn, client: client, fingerprint: fingerprint, lastUsed: time.Now()}, nil
}

// releaseSession returns a healthy connection to the idle pool. A failed send
// closes it so the next attempt dials fresh.
func (s *NotificationSMTPSender) releaseSession(session *smtpSession, healthy bool) {
	if session == nil {
		return
	}
	if !healthy {
		_ = session.client.Close()
		_ = session.conn.Close()
		return
	}
	s.sessionMu.Lock()
	if len(s.sessions) < smtpSessionPoolSize {
		s.sessions = append(s.sessions, session)
		s.sessionMu.Unlock()
		return
	}
	s.sessionMu.Unlock()
	_ = session.client.Close()
	_ = session.conn.Close()
}

// send delivers one message on the cached connection and refreshes its idle
// clock. A failed send poisons the session; the caller closes it.
func (session *smtpSession) send(ctx context.Context, from, to string, message []byte) error {
	if err := setSMTPDeadline(ctx, session.conn); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = session.conn.Close() })
	defer stop()
	if err := sendMessageWithClient(ctx, session.client, from, to, message); err != nil {
		return err
	}
	session.lastUsed = time.Now()
	return nil
}

// openSMTPClient dials and authenticates the transport for one connection.
func openSMTPClient(ctx context.Context, addr, mode string, skipTLSVerify bool) (net.Conn, *smtp.Client, error) {
	rawConn, err := utils.SafeNetDialer(smtpDialTimeout).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	if err := setSMTPDeadline(ctx, rawConn); err != nil {
		_ = rawConn.Close()
		return nil, nil, err
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	defer stopCancellation()

	conn := rawConn
	if mode == "ssl" {
		tlsConn := tls.Client(rawConn, smtpTLSConfig(addr, skipTLSVerify))
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, nil, err
		}
		conn = tlsConn
	}

	client, err := smtp.NewClient(conn, hostFromAddr(addr))
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}

	if mode == "tls" || mode == "starttls" {
		if err := client.StartTLS(smtpTLSConfig(addr, skipTLSVerify)); err != nil { //nolint:gocritic
			_ = client.Close()
			_ = conn.Close()
			return nil, nil, err
		}
	}

	if err := setSMTPDeadline(ctx, conn); err != nil {
		_ = client.Close()
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, client, nil
}

// breakerOpen reports whether the shared circuit breaker is currently blocking
// new SMTP attempts. An expired cooldown allows one probe through.
func (s *NotificationSMTPSender) breakerOpen() bool {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	if s.breakerOpenUntil.IsZero() {
		return false
	}
	if time.Now().Before(s.breakerOpenUntil) {
		return true
	}
	s.breakerOpenUntil = time.Time{}
	s.breakerFailures = 0
	return false
}

// observeDispatch updates the breaker from one send result.
func (s *NotificationSMTPSender) observeDispatch(err error) {
	if err == nil {
		s.breakerMu.Lock()
		s.breakerFailures = 0
		s.breakerOpenUntil = time.Time{}
		s.breakerMu.Unlock()
		return
	}
	if !smtpConnectionFailure(err) {
		return
	}
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	s.breakerFailures++
	if s.breakerFailures >= smtpBreakerFailureThreshold {
		s.breakerOpenUntil = time.Now().Add(smtpBreakerCooldown)
		s.breakerFailures = 0
	}
}

// smtpConnectionFailure distinguishes transport/auth failures from a
// per-recipient rejection. A 550 for one address must not stop every other
// recipient; a dial failure, timeout, TLS error, or auth rejection must.
func smtpConnectionFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var protocolErr *textproto.Error
	if errors.As(err, &protocolErr) {
		switch protocolErr.Code {
		case 421, 450, 451, 452, 454, 530, 534, 535:
			return true
		default:
			return false
		}
	}
	return true
}

// sendEmail sends a transactional (non-threaded) email using the SMTP config.
func (s *NotificationSMTPSender) sendEmail(config *models.ChannelConfig, toEmail, subject, htmlBody, textBody string) error {
	return s.sendEmailContext(context.Background(), config, toEmail, subject, htmlBody, textBody)
}

func (s *NotificationSMTPSender) sendEmailContext(ctx context.Context, config *models.ChannelConfig, toEmail, subject, htmlBody, textBody string) error {
	message, err := buildMime(mimeOptions{
		FromEmail: config.SMTPFromEmail,
		FromName:  config.SMTPFromName,
		ToEmail:   toEmail,
		Subject:   subject,
		HTMLBody:  htmlBody,
		TextBody:  textBody,
	})
	if err != nil {
		return err
	}
	return s.dispatchContext(ctx, config, toEmail, message)
}

func smtpTLSConfig(addr string, skipTLSVerify bool) *tls.Config {
	config := utils.OutboundTLSConfig(hostFromAddr(addr))
	config.InsecureSkipVerify = config.InsecureSkipVerify || skipTLSVerify //nolint:gosec // Explicit SMTP channel or process-wide opt-in.
	return config
}

// sendPlaintext sends unauthenticated email over an unencrypted connection.
// SafeNetDialer keeps the process-wide local-connection policy effective for
// this transport just as it is for TLS SMTP. It backs the SMTP conversation
// tests in core-tests.
//
//nolint:unused // Test-only entry point; production sends go through dispatchContext.
func sendPlaintextContext(ctx context.Context, addr, from, to string, message []byte) error {
	conn, client, err := openSMTPClient(ctx, addr, "none", false)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	defer func() { _ = client.Close() }()
	stopCancellation := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancellation()
	return sendMessageWithClient(ctx, client, from, to, message)
}

// sendMessageWithClient delivers one message on an already-authenticated client.
func sendMessageWithClient(ctx context.Context, client *smtp.Client, from, to string, message []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := client.Mail(from); err != nil { //nolint:gocritic
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil { //nolint:gocritic
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = writer.Write(message); err != nil {
		_ = writer.Close()
		return err
	}
	// DATA is not accepted until the writer is closed; servers report final
	// policy/recipient failures (for example 550) here. Discarding this error
	// falsely reported rejected mail as delivered.
	return writer.Close()
}

func setSMTPDeadline(ctx context.Context, conn net.Conn) error {
	deadline := time.Now().Add(smtpOperationTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	return conn.SetDeadline(deadline)
}

// SendCustomEmail sends a custom email with the provided subject and body
// This is used for transactional emails like email verification
func (s *NotificationSMTPSender) SendCustomEmail(toEmail, subject, htmlBody, textBody string) error {
	// Get SMTP configuration
	config, err := s.getSMTPConfig()
	if err != nil {
		return fmt.Errorf("failed to get SMTP config: %w", err)
	}

	// Send email
	return s.sendEmail(config, toEmail, subject, htmlBody, textBody)
}

// SendEmailWithConfig sends an email using a provided config (for testing channels)
// This allows channel test emails to use the same sending logic as production emails
func (s *NotificationSMTPSender) SendEmailWithConfig(config *models.ChannelConfig, toEmail, subject, htmlBody, textBody string) error {
	return s.sendEmail(config, toEmail, subject, htmlBody, textBody)
}

// SendThreadedEmail sends an email with RFC 5322 threading headers
// (Message-ID, In-Reply-To, References) so reply-tracking email clients
// keep the conversation grouped.
func (s *NotificationSMTPSender) SendThreadedEmail(params outbound.ThreadedEmailParams) error {
	return s.SendThreadedEmailContext(context.Background(), params)
}

// SendThreadedEmailContext is SendThreadedEmail with cancellation propagated
// through SMTP dialing, handshake, commands, and DATA completion.
func (s *NotificationSMTPSender) SendThreadedEmailContext(ctx context.Context, params outbound.ThreadedEmailParams) error {
	config, err := s.getSMTPConfig()
	if err != nil {
		return fmt.Errorf("failed to get SMTP config: %w", err)
	}
	message, err := buildMime(mimeOptions{
		FromEmail:    config.SMTPFromEmail,
		FromName:     config.SMTPFromName,
		ToEmail:      params.ToEmail,
		ToName:       params.ToName,
		ReplyToEmail: params.ReplyToEmail,
		ReplyToName:  params.ReplyToName,
		Subject:      params.Subject,
		HTMLBody:     params.HTMLBody,
		TextBody:     params.TextBody,
		MessageID:    params.MessageID,
		InReplyTo:    params.InReplyTo,
		References:   params.References,
	})
	if err != nil {
		return err
	}
	return s.dispatchContext(ctx, config, params.ToEmail, message)
}

// WithConfig returns a sender that uses the dispatcher-selected channel
// config for this send. The shared instance keeps the session pool and
// circuit breaker. Operator managed-mode override still happens inside
// dispatchContext.
func (s *NotificationSMTPSender) WithConfig(cfg *models.ChannelConfig) outbound.Sender {
	copied := models.ChannelConfig{}
	if cfg != nil {
		copied = *cfg
	}
	return &configuredSender{base: s, cfg: &copied}
}

type configuredSender struct {
	base *NotificationSMTPSender
	cfg  *models.ChannelConfig
}

func (c *configuredSender) IsConfigured() bool {
	if c == nil || c.base == nil || c.cfg == nil {
		return false
	}
	if err := operatorcontrols.From(c.base.db).Check(operatorcontrols.MailSend); err != nil {
		return false
	}
	cfg := c.cfg
	if managed := operatorcontrols.From(c.base.db).Mail(); managed != nil && managed.Mode == "managed" {
		cfg = managedSMTPConfig(managed)
	}
	return cfg.SMTPHost != "" && cfg.SMTPPort > 0 && cfg.SMTPFromEmail != ""
}

func (c *configuredSender) SendTransactional(toEmail, locale, templateName string, data any) error {
	return c.SendTransactionalContext(context.Background(), toEmail, locale, templateName, data)
}

func (c *configuredSender) SendTransactionalContext(ctx context.Context, toEmail, locale, templateName string, data any) error {
	if !c.IsConfigured() {
		return outbound.ErrNotConfigured
	}
	subject, htmlBody, textBody, err := c.base.RenderEmailLocale(locale, templateName, data)
	if err != nil {
		return fmt.Errorf("render %s: %w", templateName, err)
	}
	return c.base.sendEmailContext(ctx, c.cfg, toEmail, subject, htmlBody, textBody)
}

func (c *configuredSender) SendThreadedEmail(params outbound.ThreadedEmailParams) error {
	return c.SendThreadedEmailContext(context.Background(), params)
}

func (c *configuredSender) SendThreadedEmailContext(ctx context.Context, params outbound.ThreadedEmailParams) error {
	if !c.IsConfigured() {
		return outbound.ErrNotConfigured
	}
	message, err := buildMime(mimeOptions{
		FromEmail:    c.cfg.SMTPFromEmail,
		FromName:     c.cfg.SMTPFromName,
		ToEmail:      params.ToEmail,
		ToName:       params.ToName,
		ReplyToEmail: params.ReplyToEmail,
		ReplyToName:  params.ReplyToName,
		Subject:      params.Subject,
		HTMLBody:     params.HTMLBody,
		TextBody:     params.TextBody,
		MessageID:    params.MessageID,
		InReplyTo:    params.InReplyTo,
		References:   params.References,
	})
	if err != nil {
		return err
	}
	return c.base.dispatchContext(ctx, c.cfg, params.ToEmail, message)
}

func (c *configuredSender) RenderEmail(templateName string, data any) (string, string, string, error) {
	return c.base.RenderEmail(templateName, data)
}

func (c *configuredSender) RenderEmailLocale(locale, templateName string, data any) (string, string, string, error) {
	return c.base.RenderEmailLocale(locale, templateName, data)
}

func (c *configuredSender) SendBatchedNotifications(userEmail, userName, locale, timezone string, notifications []models.Notification) error {
	return c.SendBatchedNotificationsContext(context.Background(), userEmail, userName, locale, timezone, notifications)
}

func (c *configuredSender) SendBatchedNotificationsContext(ctx context.Context, userEmail, userName, locale, timezone string, notifications []models.Notification) error {
	if len(notifications) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.IsConfigured() {
		return outbound.ErrNotConfigured
	}
	subject, htmlBody, textBody, err := c.base.RenderEmailLocale(locale, emailutil.TemplateNotificationBatch, emailutil.BuildNotificationBatchData(c.base.baseURL, userName, locale, timezone, notifications))
	if err != nil {
		return fmt.Errorf("failed to render notification email: %w", err)
	}
	return c.base.sendEmailContext(ctx, c.cfg, userEmail, subject, htmlBody, textBody)
}
