package smtp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"windshift/internal/database"
	"windshift/internal/models"
	"windshift/internal/outbound"
)

func TestWithConfigUsesSelectedChannelNotDefaultRow(t *testing.T) {
	db, err := database.NewDatabase("sqlite", filepath.Join(t.TempDir(), "smtp.db"), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE channels (
		id INTEGER PRIMARY KEY,
		type TEXT NOT NULL,
		direction TEXT NOT NULL,
		status TEXT NOT NULL,
		is_default INTEGER NOT NULL DEFAULT 0,
		config TEXT NOT NULL DEFAULT '{}',
		updated_at TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO channels (type, direction, status, is_default, config) VALUES ('smtp', 'outbound', 'enabled', 1, ?)`,
		`{"smtp_host":"unused.invalid","smtp_port":587,"smtp_from_email":"default@example.com"}`); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	delivered := make(chan string, 1)
	serverErrors := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		t.Cleanup(func() { _ = conn.Close() })
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(conn)
		if _, err := fmt.Fprint(conn, "220 localhost ESMTP\r\n"); err != nil {
			serverErrors <- err
			return
		}
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if !errors.Is(err, io.EOF) {
					serverErrors <- err
				}
				return
			}
			response := "250 OK\r\n"
			switch {
			case strings.HasPrefix(line, "DATA"):
				if _, err := fmt.Fprint(conn, "354 End with a dot\r\n"); err != nil {
					serverErrors <- err
					return
				}
				var message strings.Builder
				for {
					part, err := reader.ReadString('\n')
					if err != nil {
						serverErrors <- err
						return
					}
					if part == ".\r\n" {
						break
					}
					message.WriteString(part)
				}
				delivered <- message.String()
			case strings.HasPrefix(line, "QUIT"):
				_, _ = fmt.Fprint(conn, "221 Bye\r\n")
				return
			}
			if _, err := fmt.Fprint(conn, response); err != nil {
				serverErrors <- err
				return
			}
		}
	}()
	host, portString, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatal(err)
	}
	sender := NewNotificationSMTPSender(db)
	selected := sender.WithConfig(&models.ChannelConfig{
		SMTPHost: host, SMTPPort: port, SMTPFromEmail: "selected@example.com",
		SMTPEncryption: "none", SMTPAuthMethod: "none",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := selected.SendThreadedEmailContext(ctx, outbound.ThreadedEmailParams{
		ToEmail: "customer@example.com", Subject: "Selected transport", TextBody: "Delivered through the selected channel.",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-delivered:
		if !strings.Contains(message, "selected@example.com") || !strings.Contains(message, "Delivered through the selected channel.") {
			t.Fatalf("unexpected delivered message: %s", message)
		}
	case err := <-serverErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	unconfigured := sender.WithConfig(&models.ChannelConfig{})
	if err := unconfigured.SendTransactionalContext(ctx, "customer@example.com", "en", "unused", nil); !errors.Is(err, outbound.ErrNotConfigured) {
		t.Fatalf("unconfigured selected sender = %v", err)
	}
}
