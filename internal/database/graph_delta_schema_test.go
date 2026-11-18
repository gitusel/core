package database

import (
	"path/filepath"
	"testing"
)

func TestGraphDeltaSchemaFreshAndUpgrade(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		name := "fresh"
		if upgrade {
			name = "upgrade"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "windshift.db")
			db, err := NewSQLiteDBWithPoolSizes(path, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if db != nil {
					_ = db.Close()
				}
			})
			if err := db.Initialize(); err != nil {
				t.Fatal(err)
			}
			var channelID, intakeID int
			if err := db.QueryRow(`INSERT INTO channels (name, type, direction) VALUES ('Graph mailbox', 'email', 'inbound') RETURNING id`).Scan(&channelID); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`INSERT INTO intakes (mailbox_id) VALUES (?) RETURNING id`, channelID).Scan(&intakeID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO email_intake_state (intake_id, last_uid, uid_validity, failed_message_uid, failed_message_uid_validity, failed_message_count) VALUES (?, 42, 99, 43, 99, 2)`, intakeID); err != nil {
				t.Fatal(err)
			}
			if upgrade {
				if _, err := db.Exec(`ALTER TABLE email_intake_state DROP COLUMN graph_delta_link`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = '20261117_intake_graph_delta'`); err != nil {
					t.Fatal(err)
				}
				if err := db.Initialize(); err != nil {
					t.Fatal(err)
				}
			}
			var uid, validity, failedUID, failedValidity, failedCount int
			var link string
			if err := db.QueryRow(`SELECT last_uid, uid_validity, failed_message_uid, failed_message_uid_validity, failed_message_count, graph_delta_link FROM email_intake_state WHERE intake_id = ?`, intakeID).Scan(&uid, &validity, &failedUID, &failedValidity, &failedCount, &link); err != nil {
				t.Fatal(err)
			}
			if uid != 42 || validity != 99 || failedUID != 43 || failedValidity != 99 || failedCount != 2 || link != "" {
				t.Fatalf("cursor after initialization = (%d, %d, %d, %d, %d, %q)", uid, validity, failedUID, failedValidity, failedCount, link)
			}
			const saved = "https://graph.microsoft.com/v1.0/users/mailbox/mailFolders/inbox/messages/delta?$deltatoken=opaque"
			if _, err := db.Exec(`UPDATE email_intake_state SET graph_delta_link = ? WHERE intake_id = ?`, saved, intakeID); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = NewSQLiteDBWithPoolSizes(path, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Initialize(); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT graph_delta_link FROM email_intake_state WHERE intake_id = ?`, intakeID).Scan(&link); err != nil {
				t.Fatal(err)
			}
			if link != saved {
				t.Fatalf("cursor after restart = %q, want %q", link, saved)
			}
			if _, err := db.Exec(`UPDATE email_intake_state SET graph_delta_link = NULL WHERE intake_id = ?`, intakeID); err == nil {
				t.Fatal("NULL Graph cursor was accepted")
			}
		})
	}
}
