package graph

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
)

// mimeMessage is the RFC 5322 message Graph sendMail accepts as base64 MIME.
// Threading headers are real Internet headers, not Graph internetMessageHeaders
// (those must start with "x-" and cannot carry Message-ID or In-Reply-To).
type mimeMessage struct {
	FromEmail, FromName  string
	ToEmail, ToName      string
	ReplyToEmail         string
	ReplyToName          string
	Subject              string
	HTMLBody, TextBody   string
	MessageID, InReplyTo string
	References           []string
}

func buildMIME(opts mimeMessage) ([]byte, error) {
	boundary, err := newMIMEBoundary()
	if err != nil {
		return nil, err
	}

	var body bytes.Buffer
	parts := multipart.NewWriter(&body)
	if err := parts.SetBoundary(boundary); err != nil {
		return nil, fmt.Errorf("set MIME boundary: %w", err)
	}
	if err := writeMIMEPart(parts, "text/plain", opts.TextBody); err != nil {
		return nil, err
	}
	if err := writeMIMEPart(parts, "text/html", opts.HTMLBody); err != nil {
		return nil, err
	}
	if err := parts.Close(); err != nil {
		return nil, fmt.Errorf("close MIME multipart: %w", err)
	}

	var message bytes.Buffer
	writeMIMEHeader(&message, "From", encodeMailbox(opts.FromName, opts.FromEmail))
	to := sanitizeHeader(opts.ToEmail)
	if opts.ToName != "" {
		to = encodeMailbox(opts.ToName, opts.ToEmail)
	}
	writeMIMEHeader(&message, "To", to)
	writeMIMEHeader(&message, "Subject", encodeHeaderWord(opts.Subject))
	writeMIMEHeader(&message, "MIME-Version", "1.0")
	writeMIMEHeader(&message, "Content-Type", mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": boundary}))

	if replyTo := strings.TrimSpace(opts.ReplyToEmail); replyTo != "" && !strings.EqualFold(replyTo, opts.FromEmail) {
		writeMIMEHeader(&message, "Reply-To", encodeMailbox(opts.ReplyToName, replyTo))
	}
	if messageID := formatMessageID(opts.MessageID); messageID != "" {
		writeMIMEHeader(&message, "Message-ID", messageID)
	}
	if inReplyTo := formatMessageID(opts.InReplyTo); inReplyTo != "" {
		writeMIMEHeader(&message, "In-Reply-To", inReplyTo)
	}
	if folded := foldReferences(opts.References); folded != "" {
		message.WriteString(folded)
	}

	message.WriteString("\r\n")
	message.Write(body.Bytes())
	return message.Bytes(), nil
}

func newMIMEBoundary() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate MIME boundary: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func writeMIMEHeader(b *bytes.Buffer, name, value string) {
	b.WriteString(name)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\r\n")
}

func writeMIMEPart(parts *multipart.Writer, mediaType, content string) error {
	header := textproto.MIMEHeader{}
	header["Content-Type"] = []string{mediaType + "; charset=UTF-8"}
	header["Content-Transfer-Encoding"] = []string{"quoted-printable"}
	writer, err := parts.CreatePart(header)
	if err != nil {
		return fmt.Errorf("create %s MIME part: %w", mediaType, err)
	}
	quoted := quotedprintable.NewWriter(writer)
	if _, err := io.WriteString(quoted, content); err != nil {
		return fmt.Errorf("write %s MIME part: %w", mediaType, err)
	}
	if err := quoted.Close(); err != nil {
		return fmt.Errorf("encode %s MIME part: %w", mediaType, err)
	}
	return nil
}

func sanitizeHeader(s string) string {
	if s == "" {
		return s
	}
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

func encodeHeaderWord(s string) string {
	s = sanitizeHeader(s)
	if s == "" {
		return s
	}
	return mime.QEncoding.Encode("utf-8", s)
}

func encodeMailbox(name, addr string) string {
	addr = sanitizeHeader(addr)
	name = sanitizeHeader(name)
	if name == "" {
		return addr
	}
	address := &mail.Address{Name: name, Address: addr}
	if address.Address == "" {
		return addr
	}
	return address.String()
}

func formatMessageID(value string) string {
	value = strings.TrimSpace(sanitizeHeader(value))
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "<"), ">"))
	if value == "" || strings.ContainsAny(value, "<> \t") {
		return ""
	}
	return "<" + value + ">"
}

func foldReferences(refs []string) string {
	clean := make([]string, 0, len(refs))
	for _, ref := range refs {
		if formatted := formatMessageID(ref); formatted != "" {
			clean = append(clean, formatted)
		}
	}
	if len(clean) == 0 {
		return ""
	}
	const softLimit = 78
	var b strings.Builder
	b.WriteString("References: ")
	column := len("References: ")
	for i, field := range clean {
		if i > 0 {
			if column+1+len(field) > softLimit {
				b.WriteString("\r\n ")
				column = 1
			} else {
				b.WriteString(" ")
				column++
			}
		}
		b.WriteString(field)
		column += len(field)
	}
	b.WriteString("\r\n")
	return b.String()
}
