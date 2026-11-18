package graph

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"windshift/internal/database"
	"windshift/internal/operatorcontrols"
	"windshift/internal/outbound"
)

func TestGraphSendHonorsOperatorPolicy(t *testing.T) {
	cases := []struct {
		name       string
		policy     operatorcontrols.Policy
		capability operatorcontrols.Capability
		budget     bool
	}{
		{name: "disabled", policy: operatorcontrols.Policy{Mail: &operatorcontrols.MailPolicy{Mode: "disabled"}}, capability: operatorcontrols.MailSend},
		{name: "suspended", policy: operatorcontrols.Policy{Mail: &operatorcontrols.MailPolicy{Mode: "tenant", Suspended: true}}, capability: operatorcontrols.MailSend},
		{name: "managed", policy: operatorcontrols.Policy{Mail: &operatorcontrols.MailPolicy{
			Mode: "managed", Host: "relay.example.com", Port: 587, Encryption: "starttls",
			FromAddress: "operator@example.com", RelayAbuseControls: true,
		}}, capability: operatorcontrols.MailTransport},
		{name: "budget unavailable", policy: operatorcontrols.Policy{Limits: operatorcontrols.Limits{MailRecipientsPerDay: 1}}, budget: true},
		{name: "approved OAuth with generic mail denied", policy: operatorcontrols.Policy{Deny: []operatorcontrols.Capability{operatorcontrols.GenericMail}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == "/token" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"access_token":"policy-token","expires_in":3600}`)
					return
				}
				w.WriteHeader(http.StatusAccepted)
			}))
			t.Cleanup(srv.Close)
			controls, err := operatorcontrols.New(tc.policy)
			if err != nil {
				t.Fatal(err)
			}
			sender := testSender(srv, graphConfig())
			sender.db = database.WithOperatorControls(nil, controls)
			err = sender.SendThreadedEmail(outbound.ThreadedEmailParams{
				ToEmail: "recipient@example.com", Subject: "Policy", TextBody: "Policy-controlled mail.",
			})
			if tc.capability != "" {
				var denied *operatorcontrols.DeniedError
				if !errors.As(err, &denied) || denied.Capability != tc.capability {
					t.Fatalf("send error = %v, want denial for %s", err, tc.capability)
				}
			} else if tc.budget {
				if !errors.Is(err, operatorcontrols.ErrBudgetUnavailable) {
					t.Fatalf("send error = %v, want unavailable budget", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if tc.capability != "" || tc.budget {
				if requests.Load() != 0 {
					t.Fatalf("denied send made %d HTTP requests", requests.Load())
				}
			} else if requests.Load() != 2 {
				t.Fatalf("approved send made %d HTTP requests, want token and submit", requests.Load())
			}
		})
	}
}
