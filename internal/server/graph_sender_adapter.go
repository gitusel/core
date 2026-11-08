package server

import (
	"context"
	"fmt"
	"time"

	"windshift/internal/email"
	"windshift/internal/smtp"
)

// graphSenderAdapter adapts email.GraphClient to the smtp.GraphSender
// interface. The token shapes differ — email.OAuthTokens carries
// ExpiresAt *time.Time (nil-able from the wire), smtp.GraphToken carries a
// value time.Time — so the adapter maps nil to a short-lived default rather
// than failing sends on a missing expiry.
type graphSenderAdapter struct {
	client *email.GraphClient
}

// graphSenderFromEmailClient wraps a GraphClient as smtp.GraphSender.
func graphSenderFromEmailClient(client *email.GraphClient) smtp.GraphSender {
	return &graphSenderAdapter{client: client}
}

func (a *graphSenderAdapter) FetchToken(ctx context.Context, tenantID, clientID, clientSecret string) (*smtp.GraphToken, error) {
	tok, err := a.client.FetchToken(ctx, tenantID, clientID, clientSecret)
	if err != nil {
		return nil, err
	}
	return graphTokenFromOAuth(tok)
}

// graphTokenFromOAuth maps email.OAuthTokens to smtp.GraphToken. A nil
// expiry maps to the zero time so the sender's cache refetches on every
// send — reusing a token of unknown lifetime for up-to-30-minutes is a
// fail-open risk. Callers must treat zero as "never reuse".
func graphTokenFromOAuth(tok *email.OAuthTokens) (*smtp.GraphToken, error) {
	if tok == nil {
		return nil, fmt.Errorf("graph adapter: nil token response")
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("graph adapter: empty access token")
	}
	var expiresAt time.Time
	if tok.ExpiresAt != nil {
		expiresAt = *tok.ExpiresAt
	}
	return &smtp.GraphToken{AccessToken: tok.AccessToken, ExpiresAt: expiresAt}, nil
}

func (a *graphSenderAdapter) SendMime(ctx context.Context, accessToken, mailboxUPN string, mime []byte) error {
	if accessToken == "" {
		return fmt.Errorf("graph adapter: empty access token")
	}
	return a.client.SendMime(ctx, accessToken, mailboxUPN, mime)
}

// Compile-time proof that the adapter satisfies the dispatch contract.
var _ smtp.GraphSender = (*graphSenderAdapter)(nil)
