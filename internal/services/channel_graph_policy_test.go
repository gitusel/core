package services

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"windshift/internal/database"
	"windshift/internal/operatorcontrols"
)

func TestGraphChannelCreationHonorsOperatorPolicy(t *testing.T) {
	cases := []struct {
		name   string
		policy operatorcontrols.Policy
		deny   bool
	}{
		{name: "transport management denied", policy: operatorcontrols.Policy{Deny: []operatorcontrols.Capability{operatorcontrols.MailTransport}}, deny: true},
		{name: "generic transport denied", policy: operatorcontrols.Policy{Deny: []operatorcontrols.Capability{operatorcontrols.GenericMail}}},
		{name: "managed relay", policy: operatorcontrols.Policy{Mail: &operatorcontrols.MailPolicy{
			Mode: "managed", Host: "relay.example.com", Port: 587, Encryption: "starttls",
			FromAddress: "operator@example.com", RelayAbuseControls: true,
		}}, deny: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := database.NewSQLiteDBWithPoolSizes(filepath.Join(t.TempDir(), "windshift.db"), 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.Initialize(); err != nil {
				t.Fatal(err)
			}
			controls, err := operatorcontrols.New(tc.policy)
			if err != nil {
				t.Fatal(err)
			}
			service := NewChannelService(database.WithOperatorControls(db, controls), nil)
			channel, err := service.Create(context.Background(), ChannelCreateRequest{
				Name: "Graph policy", Type: "graph", Direction: "outbound",
			})
			var count int
			if queryErr := db.QueryRow(`SELECT COUNT(*) FROM channels WHERE type = 'graph'`).Scan(&count); queryErr != nil {
				t.Fatal(queryErr)
			}
			if tc.deny {
				var denied *operatorcontrols.DeniedError
				if !errors.As(err, &denied) || denied.Capability != operatorcontrols.MailTransport || channel != nil || count != 0 {
					t.Fatalf("denied creation = (%#v, %v), persisted rows = %d", channel, err, count)
				}
			} else if err != nil || channel == nil || channel.Type != "graph" || count != 1 {
				t.Fatalf("approved OAuth creation = (%#v, %v), persisted rows = %d", channel, err, count)
			}
		})
	}
}
