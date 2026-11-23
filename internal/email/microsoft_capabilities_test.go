package email

import (
	"context"
	"errors"
	"testing"

	"windshift/internal/mailbox"
	"windshift/internal/models"
	"windshift/internal/teams"
)

// TestMicrosoftProviderOffersBothGraphWorkloads pins the provider shape now
// that Graph serves more than mail: one Microsoft app registration backs
// several workloads, and each is an optional capability rather than a widening
// of the mailbox contract. Teams is declared here even before it is
// implemented, so no caller can assume "Graph provider" means "mail provider"
// and so adding SharePoint files later is a third capability, not a redesign.
func TestMicrosoftProviderOffersBothGraphWorkloads(t *testing.T) {
	p := NewMicrosoftProvider("client", "secret", "tenant", nil)
	var provider Provider = p

	if _, ok := provider.(mailbox.Provider); !ok {
		t.Fatal("MicrosoftProvider must offer the mailbox capability for mail")
	}
	if _, ok := provider.(teams.Provider); !ok {
		t.Fatal("MicrosoftProvider must offer the Teams capability")
	}
}

// TestMicrosoftProviderTeamsIsExplicitlyUnavailable pins that the Teams
// capability is declared but honest: it reports that it is not implemented yet
// instead of returning a source that would silently deliver nothing. A silent
// no-op would let a channel look configured while no message ever arrives.
func TestMicrosoftProviderTeamsIsExplicitlyUnavailable(t *testing.T) {
	p := NewMicrosoftProvider("client", "secret", "tenant", nil)
	teamsProvider, ok := any(p).(teams.Provider)
	if !ok {
		t.Fatal("MicrosoftProvider does not offer the Teams capability")
	}

	_, err := teamsProvider.OpenChannel(context.Background(), &models.ChannelConfig{
		GraphTenantID:     "tenant",
		GraphClientID:     "client",
		GraphClientSecret: "secret",
	}, teams.ChannelRef{TeamID: "team-id", ChannelID: "19:abc@thread.tacv2"})
	if err == nil {
		t.Fatal("OpenChannel returned a source for an unimplemented workload")
	}
	if !errors.Is(err, teams.ErrNotAvailable) {
		t.Fatalf("err = %v, want it to wrap teams.ErrNotAvailable so callers can distinguish it from a credential failure", err)
	}
}

// TestTeamsProviderDoesNotNeedMailConfig keeps the capabilities independent: a
// Teams channel must not require a mail-from address it will never use. The
// outcome must still be the "not implemented" signal, not a configuration
// complaint about a field Teams has no use for.
func TestTeamsProviderDoesNotNeedMailConfig(t *testing.T) {
	p := NewMicrosoftProvider("client", "secret", "tenant", nil)
	teamsProvider, ok := any(p).(teams.Provider)
	if !ok {
		t.Fatal("MicrosoftProvider does not offer the Teams capability")
	}
	src, err := teamsProvider.OpenChannel(context.Background(), &models.ChannelConfig{
		GraphTenantID:     "tenant",
		GraphClientID:     "client",
		GraphClientSecret: "secret",
		// GraphFromEmail deliberately absent: it is a mail-only field.
	}, teams.ChannelRef{TeamID: "team-id", ChannelID: "19:abc@thread.tacv2"})
	if !errors.Is(err, teams.ErrNotAvailable) {
		t.Fatalf("err = %v, want ErrNotAvailable: Teams must not validate mail-only fields", err)
	}
	if src != nil {
		t.Fatalf("OpenChannel returned a %T source alongside its error; a caller could poll a channel that delivers nothing", src)
	}
}
