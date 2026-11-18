// This file contains the Microsoft Graph mail provider. It is the Graph-side
// twin of internal/smtp/notification_sender.go: it owns delivery of mail via
// Microsoft Graph's /sendMail endpoint. It shares nothing with the SMTP
// package; shared behavior (templates, notification rendering, template
// repository, secret decryption) lives in internal/emailutil and
// internal/email.
package graph

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"windshift/internal/database"
	"windshift/internal/email"
	"windshift/internal/emailutil"
	"windshift/internal/i18n"
	"windshift/internal/models"
	"windshift/internal/operatorcontrols"
	"windshift/internal/outbound"
	"windshift/internal/repository"
)

// Sender delivers mail through Microsoft Graph. It is constructed per
// outbound channel from the stored channel configuration; the client secret
// stored at rest in the channel config is decrypted here at send time.
type Sender struct {
	db         database.Database
	templates  *emailutil.Renderer
	baseURL    string
	encryption outbound.Encryptor
	cfg        models.ChannelConfig

	// testBaseURL, testTokenURL, and testHTTP redirect Graph calls. Empty
	// means the commercial endpoints and the default SSRF-safe client.
	testBaseURL  string
	testTokenURL string
	testHTTP     *http.Client
}

// NewSender builds a Graph mail sender for a channel. enc is the at-rest
// decryptor for graph_client_secret; pass nil only when the secret is stored
// in plaintext (legacy installs).
func NewSender(db database.Database, baseURL string, enc outbound.Encryptor, cfg models.ChannelConfig) *Sender {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return &Sender{
		db: db,
		templates: &emailutil.Renderer{
			Templates: repository.NewEmailTemplateRepository(db),
			BaseURL:   baseURL,
		},
		baseURL:    baseURL,
		encryption: enc,
		cfg:        cfg,
	}
}

// SetBaseURL updates the browser-visible base URL used for action links in
// digest emails. Called from server startup once the public URL is known.
func (s *Sender) SetBaseURL(baseURL string) {
	s.baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if s.templates != nil {
		s.templates.BaseURL = s.baseURL
	}
}

// SetEncryption wires the at-rest encryption service used to decrypt the
// client secret before calling Graph.
func (s *Sender) SetEncryption(enc outbound.Encryptor) {
	s.encryption = enc
}

// IsConfigured reports whether the channel carries the minimum needed to send.
// It does not perform network I/O.
func (s *Sender) IsConfigured() bool {
	cfg := s.cfg
	return strings.TrimSpace(cfg.GraphTenantID) != "" &&
		strings.TrimSpace(cfg.GraphClientID) != "" &&
		strings.TrimSpace(cfg.GraphClientSecret) != "" &&
		strings.TrimSpace(cfg.GraphFromEmail) != ""
}

// client builds a ready-to-use Graph client with the secret decrypted.
func (s *Sender) client(ctx context.Context) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	secret, err := email.DecryptOrLegacy(s.encryption, s.cfg.GraphClientSecret)
	if err != nil {
		return nil, err
	}
	cfg := Config{
		TenantID:     s.cfg.GraphTenantID,
		ClientID:     s.cfg.GraphClientID,
		ClientSecret: secret,
	}
	if s.testHTTP != nil {
		cfg.HTTPClient = s.testHTTP
		cfg.BaseURL = s.testBaseURL
		cfg.TokenURL = s.testTokenURL
	}
	return NewClient(cfg)
}

func (s *Sender) renderNamed(ctx context.Context, locale, templateName string, data any) (subject, htmlBody, textBody string, err error) {
	return s.templates.RenderLocale(locale, templateName, data)
}

func (s *Sender) renderBatch(ctx context.Context, userName, locale, timezone string, notifications []models.Notification) (subject, htmlBody, textBody string, err error) {
	return s.templates.RenderBatch(locale, timezone, userName, notifications)
}

// admit enforces operator mail policy for an actual Graph submission.
// MailSend covers disabled and suspended mail. Managed mode is rejected with
// MailTransport so the dispatcher can force the operator SMTP relay instead.
// GenericMail is not consulted: application Mail.Send is not a generic SMTP
// transport. The daily recipient budget is charged here, at submit time.
func (s *Sender) admit(ctx context.Context) error {
	controls := operatorcontrols.From(s.db)
	if err := controls.Check(operatorcontrols.MailSend); err != nil {
		return err
	}
	if managed := controls.Mail(); managed != nil && managed.Mode == "managed" {
		return &operatorcontrols.DeniedError{Capability: operatorcontrols.MailTransport}
	}
	return controls.Consume(ctx, "mail_recipients", 1, controls.Limits().MailRecipientsPerDay, 24*time.Hour)
}

func (s *Sender) sendMail(ctx context.Context, to, toName, subject, htmlBody, textBody, replyToEmail, replyToName, messageID, inReplyTo string, references []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.IsConfigured() {
		return outbound.ErrNotConfigured
	}
	client, err := s.client(ctx)
	if err != nil {
		return err
	}
	raw, err := buildMIME(mimeMessage{
		FromEmail:    s.cfg.GraphFromEmail,
		FromName:     s.cfg.GraphFromName,
		ToEmail:      to,
		ToName:       toName,
		ReplyToEmail: replyToEmail,
		ReplyToName:  replyToName,
		Subject:      subject,
		HTMLBody:     htmlBody,
		TextBody:     textBody,
		MessageID:    messageID,
		InReplyTo:    inReplyTo,
		References:   references,
	})
	if err != nil {
		return err
	}
	if err := s.admit(ctx); err != nil {
		return err
	}
	// sendMail MIME form: Content-Type text/plain, body is the entire RFC 5322
	// message base64-encoded. That is what carries Message-ID, In-Reply-To,
	// References, Reply-To, and both text parts. JSON sendMail cannot.
	body := []byte(base64.StdEncoding.EncodeToString(raw))
	path := "/users/" + url.PathEscape(strings.TrimSpace(s.cfg.GraphFromEmail)) + "/sendMail"
	return client.PostRaw(ctx, path, "text/plain", body)
}

// SendTransactional renders a named template and sends a transactional email.
func (s *Sender) SendTransactional(toEmail, locale, templateName string, data any) error {
	return s.SendTransactionalContext(context.Background(), toEmail, locale, templateName, data)
}

// SendTransactionalContext sends a transactional email with cancellation.
func (s *Sender) SendTransactionalContext(ctx context.Context, toEmail, locale, templateName string, data any) error {
	if !s.IsConfigured() {
		return outbound.ErrNotConfigured
	}
	subject, htmlBody, textBody, err := s.renderNamed(ctx, locale, templateName, data)
	if err != nil {
		return fmt.Errorf("render %s: %w", templateName, err)
	}
	return s.sendMail(ctx, toEmail, "", subject, htmlBody, textBody, "", "", "", "", nil)
}

// SendThreadedEmail sends a threaded transactional email.
func (s *Sender) SendThreadedEmail(params outbound.ThreadedEmailParams) error {
	return s.SendThreadedEmailContext(context.Background(), params)
}

// SendThreadedEmailContext sends a threaded transactional email with cancellation.
func (s *Sender) SendThreadedEmailContext(ctx context.Context, params outbound.ThreadedEmailParams) error {
	if !s.IsConfigured() {
		return outbound.ErrNotConfigured
	}
	return s.sendMail(ctx, params.ToEmail, params.ToName, params.Subject, params.HTMLBody, params.TextBody,
		params.ReplyToEmail, params.ReplyToName, params.MessageID, params.InReplyTo, params.References)
}

// SendBatchedNotifications sends a notification digest.
func (s *Sender) SendBatchedNotifications(userEmail, userName, locale, timezone string, notifications []models.Notification) error {
	return s.SendBatchedNotificationsContext(context.Background(), userEmail, userName, locale, timezone, notifications)
}

// SendBatchedNotificationsContext sends a notification digest with cancellation.
func (s *Sender) SendBatchedNotificationsContext(ctx context.Context, userEmail, userName, locale, timezone string, notifications []models.Notification) error {
	if len(notifications) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.IsConfigured() {
		return outbound.ErrNotConfigured
	}
	subject, htmlBody, textBody, err := s.renderBatch(ctx, userName, locale, timezone, notifications)
	if err != nil {
		return fmt.Errorf("render notification batch: %w", err)
	}
	return s.sendMail(ctx, userEmail, userName, subject, htmlBody, textBody, "", "", "", "", nil)
}

// RenderEmail renders a named template in the default locale.
func (s *Sender) RenderEmail(templateName string, data any) (string, string, string, error) {
	return s.RenderEmailLocale(i18n.Default, templateName, data)
}

// RenderEmailLocale renders a named template bound to a recipient locale.
func (s *Sender) RenderEmailLocale(locale, templateName string, data any) (string, string, string, error) {
	return s.templates.RenderLocale(locale, templateName, data)
}

// SendTestMail sends a pre-rendered message with an explicit channel config.
// db supplies operator controls so a direct channel test still honors
// MailSend, rejects managed mode, and charges the recipient budget.
func SendTestMail(ctx context.Context, db database.Database, enc outbound.Encryptor, cfg models.ChannelConfig, to, subject, htmlBody, textBody string) error {
	s := &Sender{db: db, encryption: enc, cfg: cfg}
	if !s.IsConfigured() {
		return outbound.ErrNotConfigured
	}
	return s.sendMail(ctx, to, "", subject, htmlBody, textBody, "", "", "", "", nil)
}
