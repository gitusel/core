// Package outbound routes outbound mail to the provider configured in the
// database (SMTP or Microsoft Graph). The gateway implements the same
// outbound.Sender contract as the providers and fans out to the provider
// selected by the default outbound channel row.
//
// Nothing here imports internal/smtp or internal/graph for their internals;
// the providers are constructed through small adapter closures supplied by
// the caller, so this package never needs to know how a provider builds its
// transport. Graph and SMTP depend on this package, never on each other.
package outbound

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"windshift/internal/database"
	"windshift/internal/emailutil"
	"windshift/internal/models"
	"windshift/internal/operatorcontrols"
	"windshift/internal/repository"
)

// Adapter builds a concrete Sender for a channel config. Providers register
// themselves via RegisterType; the dispatcher calls the matching adapter to
// create a per-send sender (so secrets are decrypted at send time).
type Adapter func(cfg *models.ChannelConfig) (Sender, error)

// Registry maps channel type to provider adapter. Registration happens from
// the owning package's init() or from main, so this package has no hard
// dependencies on smtp or graph.
type Registry struct {
	adapters map[string]Adapter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{adapters: map[string]Adapter{}}
}

// Register records an adapter for a channel type.
func (r *Registry) Register(channelType string, a Adapter) {
	r.adapters[channelType] = a
}

// Dispatch is the runtime switchboard: it resolves the default outbound
// channel and invokes the adapter for that channel type.
type Dispatch struct {
	db       database.Database
	baseURL  string
	registry *Registry
}

// NewDispatch returns a Dispatch ready to serve. baseURL is the
// browser-visible base URL passed to providers that render absolute links.
func NewDispatch(db database.Database, baseURL string, registry *Registry) *Dispatch {
	return &Dispatch{db: db, baseURL: baseURL, registry: registry}
}

// IsConfigured reports whether the selected outbound mail channel can send.
// It performs no network I/O. A default webhook or other non-mail channel
// does not hide a configured smtp or graph channel.
func (d *Dispatch) IsConfigured() bool {
	s, err := d.sender()
	if err != nil {
		return false
	}
	return s.IsConfigured()
}

// IsSMTPConfigured mirrors IsConfigured for services' sender interfaces, which
// still use the pre-dispatcher spelling.
func (d *Dispatch) IsSMTPConfigured() bool { return d.IsConfigured() }

// BaseURL returns the site base URL used by providers that render links.
func (d *Dispatch) BaseURL() string { return d.baseURL }

// SetBaseURL records the browser-visible base URL. Startup learns it after
// the dispatcher is constructed, so providers read it at send time via the
// adapter closure, and template rendering reads it here.
func (d *Dispatch) SetBaseURL(baseURL string) {
	d.baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

func (d *Dispatch) renderer() *emailutil.Renderer {
	return &emailutil.Renderer{
		Templates: repository.NewEmailTemplateRepository(d.db),
		BaseURL:   d.baseURL,
	}
}

// RenderEmail renders a named template in the default locale. Rendering does
// not require a configured outbound channel: templates are shared.
func (d *Dispatch) RenderEmail(templateName string, data any) (string, string, string, error) {
	return d.renderer().Render(templateName, data)
}

// RenderEmailLocale renders a named template bound to a recipient locale.
func (d *Dispatch) RenderEmailLocale(locale, templateName string, data any) (string, string, string, error) {
	return d.renderer().RenderLocale(locale, templateName, data)
}

func (d *Dispatch) loadDefaultConfig() (string, *models.ChannelConfig, error) {
	if d == nil || d.db == nil || d.registry == nil {
		return "", nil, ErrNotConfigured
	}
	types := d.registry.types()
	if len(types) == 0 {
		return "", nil, ErrNotConfigured
	}
	placeholders := make([]string, len(types))
	args := make([]any, len(types))
	for i, typ := range types {
		placeholders[i] = "?"
		args[i] = typ
	}
	in := strings.Join(placeholders, ",")
	var channelType, configJSON string
	err := d.db.QueryRow(`
		SELECT type, COALESCE(config, '{}') FROM channels
		WHERE direction = 'outbound' AND status = 'enabled' AND is_default = true
		  AND type IN (`+in+`)
		ORDER BY updated_at DESC
		LIMIT 1
	`, args...).Scan(&channelType, &configJSON)
	if errors.Is(err, sql.ErrNoRows) {
		// Legacy installs enabled SMTP without setting is_default. Prefer that
		// over a newer non-default graph row, but never a webhook.
		err = d.db.QueryRow(`
			SELECT type, COALESCE(config, '{}') FROM channels
			WHERE direction = 'outbound' AND status = 'enabled'
			  AND type IN (`+in+`)
			ORDER BY CASE type WHEN 'smtp' THEN 0 ELSE 1 END, updated_at DESC
			LIMIT 1
		`, args...).Scan(&channelType, &configJSON)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotConfigured
	}
	if err != nil {
		return "", nil, fmt.Errorf("no outbound channel configured: %w", err)
	}
	var cfg models.ChannelConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return "", nil, fmt.Errorf("failed to parse outbound channel config: %w", err)
	}
	return channelType, &cfg, nil
}

func (r *Registry) types() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.adapters))
	for typ := range r.adapters {
		out = append(out, typ)
	}
	return out
}

// buildWithType constructs the provider for the channel type.
func (r *Registry) buildWithType(channelType string, cfg *models.ChannelConfig) (Sender, error) {
	if r == nil {
		return nil, fmt.Errorf("no provider registered for channel type %q", channelType)
	}
	a, ok := r.adapters[channelType]
	if !ok {
		return nil, fmt.Errorf("no provider registered for channel type %q", channelType)
	}
	return a(cfg)
}

func (d *Dispatch) sender() (Sender, error) {
	if d == nil || d.registry == nil {
		return nil, ErrNotConfigured
	}
	controls := operatorcontrols.From(d.db)
	if err := controls.Check(operatorcontrols.MailSend); err != nil {
		return nil, err
	}
	// Operator-managed mail is an SMTP relay. A tenant Graph default must not
	// bypass it. GenericMail is enforced by the SMTP sender only; approved
	// Graph OAuth is not a generic-mail transport.
	if managed := controls.Mail(); managed != nil && managed.Mode == "managed" {
		return d.registry.buildWithType("smtp", managedMailConfig(managed))
	}
	typ, cfg, err := d.loadDefaultConfig()
	if err != nil {
		return nil, err
	}
	return d.registry.buildWithType(typ, cfg)
}

func managedMailConfig(policy *operatorcontrols.MailPolicy) *models.ChannelConfig {
	return &models.ChannelConfig{
		SMTPHost:       policy.Host,
		SMTPPort:       policy.Port,
		SMTPEncryption: policy.Encryption,
		SMTPUsername:   policy.Username,
		SMTPPassword:   policy.Password,
		SMTPAuthMethod: "auto",
		SMTPFromEmail:  policy.FromAddress,
		SMTPFromName:   policy.FromName,
	}
}

// resolveAndRun is the shared shape for every send method.
func (d *Dispatch) resolveAndRun(fn func(s Sender) error) error {
	s, err := d.sender()
	if err != nil {
		return err
	}
	return fn(s)
}

func (d *Dispatch) SendTransactional(toEmail, locale, templateName string, data any) error {
	return d.resolveAndRun(func(s Sender) error { return s.SendTransactional(toEmail, locale, templateName, data) })
}
func (d *Dispatch) SendTransactionalContext(ctx context.Context, toEmail, locale, templateName string, data any) error {
	return d.resolveAndRun(func(s Sender) error {
		return s.SendTransactionalContext(ctx, toEmail, locale, templateName, data)
	})
}
func (d *Dispatch) SendThreadedEmail(params ThreadedEmailParams) error {
	return d.resolveAndRun(func(s Sender) error { return s.SendThreadedEmail(params) })
}
func (d *Dispatch) SendThreadedEmailContext(ctx context.Context, params ThreadedEmailParams) error {
	return d.resolveAndRun(func(s Sender) error { return s.SendThreadedEmailContext(ctx, params) })
}
func (d *Dispatch) SendBatchedNotifications(userEmail, userName, locale, timezone string, notifications []models.Notification) error {
	return d.resolveAndRun(func(s Sender) error {
		return s.SendBatchedNotifications(userEmail, userName, locale, timezone, notifications)
	})
}
func (d *Dispatch) SendBatchedNotificationsContext(ctx context.Context, userEmail, userName, locale, timezone string, notifications []models.Notification) error {
	return d.resolveAndRun(func(s Sender) error {
		return s.SendBatchedNotificationsContext(ctx, userEmail, userName, locale, timezone, notifications)
	})
}
