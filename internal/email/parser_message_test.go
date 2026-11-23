package email

import (
	"strings"
	"testing"

	"windshift/internal/mailmsg"
)

// TestParseMessageNeedsNoIMAPEnvelope pins the contract that lets a Graph or
// Teams mailbox share the parser without pretending to be IMAP: everything the
// intake path needs must come from the raw RFC 5322 bytes alone. IMAP's
// envelope is a server-derived hint, not a requirement.
func TestParseMessageNeedsNoIMAPEnvelope(t *testing.T) {
	raw := []byte("From: Customer Service <customer@example.com>\r\n" +
		"To: Support <support@example.com>\r\n" +
		"Subject: Prüfung request\r\n" +
		"Date: Fri, 17 Jul 2026 10:00:00 +0000\r\n" +
		"Message-ID: <abc@example.com>\r\n" +
		"In-Reply-To: <parent@example.com>\r\n" +
		"References: <root@example.com> <parent@example.com>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"First line\r\nSecond line\r\n")

	parsed := NewParser().ParseMessage(&mailmsg.Message{Ref: "AAMk-graph-id", Raw: raw})

	if got, want := parsed.Subject, "Prüfung request"; got != want {
		t.Fatalf("subject = %q, want %q", got, want)
	}
	if got, want := parsed.MessageID, "<abc@example.com>"; got != want {
		t.Fatalf("message id = %q, want %q", got, want)
	}
	if got, want := parsed.InReplyTo, "<parent@example.com>"; got != want {
		t.Fatalf("in-reply-to = %q, want %q", got, want)
	}
	if len(parsed.References) != 2 {
		t.Fatalf("references = %v, want two", parsed.References)
	}
	if got, want := parsed.From.Address, "customer@example.com"; got != want {
		t.Fatalf("from = %q, want %q", got, want)
	}
	if got, want := parsed.From.Name, "Customer Service"; got != want {
		t.Fatalf("from name = %q, want %q", got, want)
	}
	if parsed.Date.IsZero() {
		t.Fatal("date was not recovered from raw headers")
	}
	if !strings.Contains(parsed.PlainBody, "First line") {
		t.Fatalf("plain body = %q, want it to contain the message text", parsed.PlainBody)
	}
	if len(parsed.RawHeaders) == 0 {
		t.Fatal("raw headers were not captured")
	}
}

// TestParseMessageSurvivesEmptyRaw keeps the deferred-message path honest: an
// empty payload must yield an empty result, not a panic.
func TestParseMessageSurvivesEmptyRaw(t *testing.T) {
	parsed := NewParser().ParseMessage(&mailmsg.Message{Ref: "x"})
	if parsed == nil {
		t.Fatal("ParseMessage returned nil for an empty message")
	}
	if parsed.Subject != "" || parsed.PlainBody != "" {
		t.Fatalf("empty message produced content: %#v", parsed)
	}
}

// TestMessageRefSurvivesParse proves the neutral ref is what downstream dedup
// and acknowledgement key on: parsing does not need a UID.
func TestMessageRefSurvivesParse(t *testing.T) {
	m := &mailmsg.Message{Ref: "AAMk-two", Raw: []byte("From: b@example.com\r\nSubject: s\r\nMessage-ID: <m2@example.com>\r\n\r\nbody\r\n")}
	parsed := NewParser().ParseMessage(m)
	if parsed.MessageID != "<m2@example.com>" {
		t.Fatalf("message id = %q", parsed.MessageID)
	}
	if parsed.UID != 0 {
		t.Fatalf("parsed UID = %d, want 0: a non-IMAP message has no UID", parsed.UID)
	}
}
