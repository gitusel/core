package email

import (
	"context"
	"strings"
	"testing"

	"windshift/internal/mailbox"
	"windshift/internal/models"
)

// TestMicrosoftProviderOpensNativeMailbox pins that the Microsoft provider can
// hand back a Mailbox directly, without routing Graph through the IMAP-shaped
// adapter. A transport that is Graph must not have to pretend it owns a UID
// space just to be pollable.
func TestMicrosoftProviderOpensNativeMailbox(t *testing.T) {
	p := NewMicrosoftProvider("client", "secret", "tenant", nil)
	var provider Provider = p
	mp, ok := provider.(mailbox.Provider)
	if !ok {
		t.Fatal("MicrosoftProvider does not implement MailboxProvider")
	}
	// An empty token is rejected before any network call, so this exercises the
	// wiring without reaching Graph.
	_, err := mp.OpenMailbox(context.Background(), &models.ChannelConfig{})
	if err == nil {
		t.Fatal("OpenMailbox accepted a config with no access token")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Fatalf("err = %v, want a missing-token error", err)
	}
}

// TestMicrosoftProviderRefusesIMAPConnect pins that the Microsoft provider no
// longer dresses Graph up as an IMAP client. Connect stays on the Provider
// interface because IMAP-backed providers need it, but a Graph mailbox must be
// reached through OpenMailbox.
func TestMicrosoftProviderRefusesIMAPConnect(t *testing.T) {
	p := NewMicrosoftProvider("client", "secret", "tenant", nil)
	client, err := p.Connect(context.Background(), &models.ChannelConfig{
		EmailOAuthAccessToken: "not-a-jwt",
		EmailOAuthEmail:       "box@contoso.example",
	})
	if err == nil {
		t.Fatal("Connect returned a client for a Graph mailbox; Graph is polled through OpenMailbox")
	}
	if client != nil {
		t.Fatalf("Connect returned %T alongside an error", client)
	}
	if !strings.Contains(err.Error(), "Mailbox") {
		t.Fatalf("err = %v, want it to point at the Mailbox path", err)
	}
}
