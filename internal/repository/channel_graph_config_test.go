package repository

import (
	"path/filepath"
	"strings"
	"testing"

	"windshift/internal/database"
	"windshift/internal/models"
)

func TestScrubChannelConfigRemovesGraphSecret(t *testing.T) {
	channel := &models.Channel{
		Config: `{"graph_client_id":"app","graph_client_secret":"enc:v1:stored","smtp_password":"secret","graph_from_email":"box@contoso.example"}`,
	}
	ScrubChannelForResponse(channel)
	if !channel.HasGraphClientSecret {
		t.Fatal("expected has_graph_client_secret")
	}
	if strings.Contains(channel.Config, "graph_client_secret") || strings.Contains(channel.Config, "smtp_password") {
		t.Fatalf("secret leaked in API config: %s", channel.Config)
	}
	if !strings.Contains(channel.Config, "graph_client_id") || !strings.Contains(channel.Config, "graph_from_email") {
		t.Fatalf("non-secret Graph fields were removed: %s", channel.Config)
	}
}

func TestPromoteDefaultOutboundSelectsGraph(t *testing.T) {
	db, err := database.NewSQLiteDBWithPoolSizes(filepath.Join(t.TempDir(), "windshift.db"), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Initialize(); err != nil {
		t.Fatal(err)
	}

	var smtpID int
	err = db.QueryRow(`SELECT id FROM channels WHERE type = 'smtp' AND direction = 'outbound' ORDER BY id LIMIT 1`).Scan(&smtpID)
	if err != nil {
		smtpID, err = insertOutboundChannel(db, "Notification Mail", "smtp", true)
		if err != nil {
			t.Fatal(err)
		}
	} else if _, err = db.ExecWrite(`UPDATE channels SET is_default = true, status = 'enabled' WHERE id = ?`, smtpID); err != nil {
		t.Fatal(err)
	}

	graphID, err := insertOutboundChannel(db, "Graph mail", "graph", false)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewChannelRepository(db)
	if err := database.WithTx(db, func(tx database.Tx) error {
		return repo.PromoteDefaultOutbound(t.Context(), tx, graphID)
	}); err != nil {
		t.Fatal(err)
	}
	if channelIsDefault(t, db, smtpID) {
		t.Fatal("SMTP remained the default after Graph promotion")
	}
	if !channelIsDefault(t, db, graphID) {
		t.Fatal("Graph channel was not selected as the default")
	}
}

func insertOutboundChannel(db database.Database, name, channelType string, isDefault bool) (int, error) {
	var id int
	err := db.QueryRow(
		`INSERT INTO channels (name, type, direction, status, is_default, config) VALUES (?, ?, 'outbound', 'enabled', ?, '{}') RETURNING id`,
		name, channelType, isDefault,
	).Scan(&id)
	return id, err
}

func channelIsDefault(t *testing.T, db database.Database, id int) bool {
	t.Helper()
	var isDefault bool
	if err := db.QueryRow(`SELECT COALESCE(is_default, false) FROM channels WHERE id = ?`, id).Scan(&isDefault); err != nil {
		t.Fatal(err)
	}
	return isDefault
}
