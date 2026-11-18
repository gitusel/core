package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"windshift/internal/database"
	"windshift/internal/models"
)

func TestDispatchIgnoresWebhookDefault(t *testing.T) {
	db := openChannelsDB(t)
	insertChannel(t, db, "webhook", true, `{}`)
	smtpCfg := models.ChannelConfig{SMTPHost: "mail.example", SMTPPort: 587, SMTPFromEmail: "from@example.com"}
	insertChannel(t, db, "smtp", false, mustJSON(smtpCfg))
	insertChannel(t, db, "graph", false, mustJSON(models.ChannelConfig{
		GraphTenantID: "t", GraphClientID: "c", GraphClientSecret: "s", GraphFromEmail: "g@example.com",
	}))

	var gotType string
	var got *models.ChannelConfig
	d := dispatchWith(db, func(typ string, cfg *models.ChannelConfig) Sender {
		gotType = typ
		got = cfg
		return &recordingSender{configured: true}
	})
	if err := d.SendTransactional("a@b.c", "en", "magic_link", nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotType != "smtp" {
		t.Fatalf("selected %q, want smtp (webhook must not mask legacy smtp)", gotType)
	}
	if got == nil || got.SMTPHost != "mail.example" || got.SMTPFromEmail != "from@example.com" {
		t.Fatalf("selected config = %+v, want the smtp row", got)
	}
}

func TestDispatchPrefersDefaultGraphOverWebhook(t *testing.T) {
	db := openChannelsDB(t)
	insertChannel(t, db, "webhook", true, `{}`)
	insertChannel(t, db, "smtp", false, mustJSON(models.ChannelConfig{SMTPHost: "mail.example", SMTPPort: 25, SMTPFromEmail: "s@example.com"}))
	insertChannel(t, db, "graph", true, mustJSON(models.ChannelConfig{
		GraphTenantID: "tenant", GraphClientID: "client", GraphClientSecret: "secret", GraphFromEmail: "graph@contoso.com",
	}))

	var gotType string
	var got *models.ChannelConfig
	d := dispatchWith(db, func(typ string, cfg *models.ChannelConfig) Sender {
		gotType = typ
		got = cfg
		return &recordingSender{configured: true}
	})
	if !d.IsConfigured() {
		t.Fatal("default graph channel reported unconfigured")
	}
	if err := d.SendBatchedNotifications("u@example.com", "User", "en", "UTC", []models.Notification{{ID: 1}}); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if gotType != "graph" || got == nil || got.GraphFromEmail != "graph@contoso.com" {
		t.Fatalf("selected %s %+v, want default graph", gotType, got)
	}
}

func TestDispatchMissingMailChannelIsSentinel(t *testing.T) {
	db := openChannelsDB(t)
	insertChannel(t, db, "webhook", true, `{}`)
	d := dispatchWith(db, func(string, *models.ChannelConfig) Sender {
		t.Fatal("adapter called with no mail channel")
		return nil
	})
	err := d.SendThreadedEmail(ThreadedEmailParams{ToEmail: "a@b.c", Subject: "s"})
	if !errors.Is(err, ErrNotConfigured) || err != ErrNotConfigured {
		t.Fatalf("err = %v, want exact ErrNotConfigured", err)
	}
	if d.IsConfigured() {
		t.Fatal("webhook-only outbound reported configured")
	}
}

type recordingSender struct{ configured bool }

func (r *recordingSender) IsConfigured() bool { return r.configured }
func (r *recordingSender) SendTransactional(string, string, string, any) error {
	return nil
}
func (r *recordingSender) SendTransactionalContext(context.Context, string, string, string, any) error {
	return nil
}
func (r *recordingSender) SendThreadedEmail(ThreadedEmailParams) error { return nil }
func (r *recordingSender) SendThreadedEmailContext(context.Context, ThreadedEmailParams) error {
	return nil
}
func (r *recordingSender) RenderEmail(string, any) (string, string, string, error) {
	return "", "", "", nil
}
func (r *recordingSender) RenderEmailLocale(string, string, any) (string, string, string, error) {
	return "", "", "", nil
}
func (r *recordingSender) SendBatchedNotifications(string, string, string, string, []models.Notification) error {
	return nil
}
func (r *recordingSender) SendBatchedNotificationsContext(context.Context, string, string, string, string, []models.Notification) error {
	return nil
}

func dispatchWith(db database.Database, build func(string, *models.ChannelConfig) Sender) *Dispatch {
	reg := NewRegistry()
	reg.Register("smtp", func(cfg *models.ChannelConfig) (Sender, error) {
		return build("smtp", cfg), nil
	})
	reg.Register("graph", func(cfg *models.ChannelConfig) (Sender, error) {
		return build("graph", cfg), nil
	})
	return NewDispatch(db, "https://app.example", reg)
}

func openChannelsDB(t *testing.T) database.Database {
	t.Helper()
	db, err := database.NewDatabase("sqlite", filepath.Join(t.TempDir(), "channels.db"), 1, 1)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE channels (
		id INTEGER PRIMARY KEY,
		type TEXT NOT NULL,
		direction TEXT NOT NULL,
		status TEXT NOT NULL,
		is_default INTEGER NOT NULL DEFAULT 0,
		config TEXT NOT NULL DEFAULT '{}',
		updated_at TEXT NOT NULL DEFAULT '2026-01-01 00:00:00'
	)`); err != nil {
		t.Fatalf("create channels: %v", err)
	}
	return db
}

func insertChannel(t *testing.T, db database.Database, channelType string, isDefault bool, config string) {
	t.Helper()
	flag := 0
	if isDefault {
		flag = 1
	}
	if _, err := db.Exec(`INSERT INTO channels (type, direction, status, is_default, config, updated_at) VALUES (?, 'outbound', 'enabled', ?, ?, ?)`,
		channelType, flag, config, "2026-01-01 00:00:0"+channelType[:1]); err != nil {
		t.Fatalf("insert %s: %v", channelType, err)
	}
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}
