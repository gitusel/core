package models

import (
	"encoding/json"
	"testing"
	"time"
)

func TestChannelConfigGraphFieldsRoundTrip(t *testing.T) {
	expires := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	cfg := ChannelConfig{
		GraphSendProvider:   "graph",
		GraphTenantID:       "01234567-89ab-cdef-0123-456789abcdef",
		GraphClientID:       "client-abc",
		GraphClientSecret:   "super-secret",
		GraphMailboxUPN:     "windshift@contoso.com",
		GraphTokenExpiresAt: &expires,
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ChannelConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.GraphSendProvider != "graph" || got.GraphTenantID != cfg.GraphTenantID ||
		got.GraphClientID != "client-abc" || got.GraphClientSecret != "super-secret" ||
		got.GraphMailboxUPN != "windshift@contoso.com" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.GraphTokenExpiresAt == nil || !got.GraphTokenExpiresAt.Equal(expires) {
		t.Fatalf("token expiry round-trip: got %v", got.GraphTokenExpiresAt)
	}
}

func TestSendProvider(t *testing.T) {
	tests := []struct {
		name string
		cfg  ChannelConfig
		want string
	}{
		{"empty defaults to smtp", ChannelConfig{}, "smtp"},
		{"legacy smtp config defaults to smtp", ChannelConfig{SMTPHost: "smtp.office365.com"}, "smtp"},
		{"graph explicit", ChannelConfig{GraphSendProvider: "graph"}, "graph"},
		{"unknown value falls back to smtp", ChannelConfig{GraphSendProvider: "carrier-pigeon"}, "smtp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.SendProvider(); got != tt.want {
				t.Errorf("SendProvider() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateGraphSendConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ChannelConfig
		wantErr string // empty means must pass
	}{
		{
			name: "smtp provider needs no graph fields",
			cfg:  ChannelConfig{SMTPHost: "smtp.example.com", GraphSendProvider: "smtp"},
		},
		{
			name:    "graph provider missing tenant",
			cfg:     ChannelConfig{GraphSendProvider: "graph", GraphClientID: "c", GraphClientSecret: "s", GraphMailboxUPN: "m@x.com"},
			wantErr: "graph send requires a tenant id",
		},
		{
			name:    "graph provider missing client id",
			cfg:     ChannelConfig{GraphSendProvider: "graph", GraphTenantID: "t", GraphClientSecret: "s", GraphMailboxUPN: "m@x.com"},
			wantErr: "graph send requires a client id",
		},
		{
			name:    "graph provider missing secret",
			cfg:     ChannelConfig{GraphSendProvider: "graph", GraphTenantID: "t", GraphClientID: "c", GraphMailboxUPN: "m@x.com"},
			wantErr: "graph send requires a client secret",
		},
		{
			name:    "graph provider missing mailbox",
			cfg:     ChannelConfig{GraphSendProvider: "graph", GraphTenantID: "t", GraphClientID: "c", GraphClientSecret: "s"},
			wantErr: "graph send requires a mailbox upn to send as",
		},
		{
			name: "complete graph config passes",
			cfg:  ChannelConfig{GraphSendProvider: "graph", GraphTenantID: "t", GraphClientID: "c", GraphClientSecret: "s", GraphMailboxUPN: "m@x.com"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
