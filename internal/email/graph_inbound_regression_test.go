package email

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"windshift/internal/models"
)

func TestGraphDeltaReplayKeepsCursorUntilMIMESucceeds(t *testing.T) {
	const token = "delegated-graph-token"
	const mailbox = "box"
	saved := "/users/" + mailbox + "/mailFolders/inbox/messages/delta?$deltatoken=saved"
	next := "/users/" + mailbox + "/mailFolders/inbox/messages/delta?$deltatoken=advanced"
	var mu sync.Mutex
	var calls []string
	mimeFails := true
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		failMIME := mimeFails
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/mailFolders/") && !strings.Contains(r.URL.Path, "/messages/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"inbox"}`)
		case r.URL.RequestURI() == saved:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, graphDeltaJSON([]graphDeltaRow{{ID: "AAMk-new", Received: "2026-07-17T08:00:00Z"}}, "", srv.URL+next))
		case r.URL.RequestURI() == next:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, graphDeltaJSON(nil, "", srv.URL+next))
		case strings.HasSuffix(r.URL.Path, "/$value"):
			if failMIME {
				http.Error(w, "unavailable", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "message/rfc822")
			_, _ = io.WriteString(w, graphTestMIME("AAMk-new", "Replay me"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	client := openGraphTestClient(t, srv, token, mailbox)
	if _, err := client.SelectMailbox("INBOX"); err != nil {
		t.Fatalf("SelectMailbox: %v", err)
	}
	client.(GraphDeltaStore).RestoreGraphDelta(srv.URL + saved)

	if _, err := client.FetchMessages(0, 2); err == nil {
		t.Fatal("failed MIME fetch succeeded")
	}
	if got := graphResumeURL(client.(GraphDeltaStore).GraphDelta()); got != srv.URL+saved {
		t.Fatalf("cursor after failed tick = %q, want the previous link", got)
	}

	mu.Lock()
	mimeFails = false
	mu.Unlock()
	messages, err := client.FetchMessages(0, 2)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(messages) != 1 || messages[0].FetchError != nil || !bytesContains(messages[0].Raw, "Replay me") {
		t.Fatalf("replay messages = %#v", messages)
	}
	if got := graphResumeURL(client.(GraphDeltaStore).GraphDelta()); got != srv.URL+next {
		t.Fatalf("cursor after success = %q, want advanced link", got)
	}

	mu.Lock()
	before := len(calls)
	mu.Unlock()
	if _, err := client.FetchMessages(0, 2); err != nil {
		t.Fatalf("advanced poll: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	savedGets := 0
	for _, call := range got[:before] {
		if call == "GET "+saved {
			savedGets++
		}
		if strings.Contains(call, "deltatoken=advanced") {
			t.Fatalf("advanced link was used before the failed tick was replayed: %s", call)
		}
	}
	if savedGets != 2 {
		t.Fatalf("saved delta fetches before advance = %d, want 2 (failed tick plus replay)\n%s", savedGets, strings.Join(got, "\n"))
	}
	if len(got[before:]) != 1 || got[before:][0] != "GET "+next {
		t.Fatalf("poll after advance = %s, want one GET of the new link", strings.Join(got[before:], "\n"))
	}
}

func TestGraphIntakesDoNotShareDeltaCursor(t *testing.T) {
	const token = "delegated-graph-token"
	const mailbox = "box"
	inboxLink := "/users/" + mailbox + "/mailFolders/inbox/messages/delta?$deltatoken=intake-a"
	inboxNext := "/users/" + mailbox + "/mailFolders/inbox/messages/delta?$deltatoken=intake-a-next"
	archiveLink := "/users/" + mailbox + "/mailFolders/archive/messages/delta?$deltatoken=intake-b"
	var calls []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.RequestURI())
		switch {
		case strings.Contains(r.URL.Path, "/mailFolders/") && !strings.Contains(r.URL.Path, "/messages/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"folder"}`)
		case r.URL.RequestURI() == inboxLink:
			_, _ = io.WriteString(w, graphDeltaJSON([]graphDeltaRow{{ID: "AAMk-a", Received: "2026-07-17T08:00:00Z"}}, "", srv.URL+inboxNext))
		case r.URL.RequestURI() == archiveLink:
			_, _ = io.WriteString(w, graphDeltaJSON([]graphDeltaRow{{ID: "AAMk-b", Received: "2026-07-17T08:00:00Z"}}, "", srv.URL+archiveLink))
		case strings.HasSuffix(r.URL.Path, "/$value"):
			w.Header().Set("Content-Type", "message/rfc822")
			_, _ = io.WriteString(w, graphTestMIME("id", "subject"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	client := openGraphTestClient(t, srv, token, mailbox)
	store := client.(GraphDeltaStore)
	if _, err := client.SelectMailbox("INBOX"); err != nil {
		t.Fatalf("select inbox: %v", err)
	}
	store.RestoreGraphDelta(srv.URL + inboxLink)
	if _, err := client.FetchMessages(0, 10); err != nil {
		t.Fatalf("inbox poll: %v", err)
	}
	if graphResumeURL(store.GraphDelta()) != srv.URL+inboxNext {
		t.Fatalf("inbox cursor = %q", store.GraphDelta())
	}

	// The scheduler selects the next intake before restoring that intake's link.
	if _, err := client.SelectMailbox("Archive"); err != nil {
		t.Fatalf("select archive: %v", err)
	}
	if store.GraphDelta() != "" {
		t.Fatalf("archive select kept inbox cursor %q", store.GraphDelta())
	}
	store.RestoreGraphDelta(srv.URL + archiveLink)
	if _, err := client.FetchMessages(0, 10); err != nil {
		t.Fatalf("archive poll: %v", err)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, archiveLink) {
		t.Fatalf("archive link was not fetched\n%s", joined)
	}
	if strings.Contains(joined, "intake-a-next") {
		t.Fatalf("archive poll reused the inbox resume link\n%s", joined)
	}

	// Two intakes on the same folder still get their own restored link.
	if _, err := client.SelectMailbox("INBOX"); err != nil {
		t.Fatalf("reselect inbox: %v", err)
	}
	if store.GraphDelta() != "" {
		t.Fatalf("same-folder select kept the previous intake cursor %q", store.GraphDelta())
	}
	store.RestoreGraphDelta(srv.URL + inboxLink)
	if graphResumeURL(store.GraphDelta()) != srv.URL+inboxLink {
		t.Fatalf("restore = %q", store.GraphDelta())
	}
}

func TestGraphDeltaRemovedAndReadNoticesAreNotIngested(t *testing.T) {
	const token = "delegated-graph-token"
	const mailbox = "inbox/tag@contoso.example"
	saved := "/users/" + url.PathEscape(mailbox) + "/mailFolders/inbox/messages/delta?$deltatoken=saved"
	advanced := saved + "-next"
	var calls []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch {
		case strings.Contains(r.URL.Path, "/mailFolders/") && !strings.Contains(r.URL.Path, "/messages/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"inbox"}`)
		case r.URL.RequestURI() == saved:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"value": [
					{"@odata.type":"#microsoft.graph.message","id":"AAMk-deleted","@removed":{"reason":"deleted"}},
					{"@odata.type":"#microsoft.graph.message","id":"AAMk-moved","@removed":{"reason":"changed"}},
					{"@odata.type":"#microsoft.graph.message","id":"AAMk-read","isRead":true},
					{"@odata.type":"#microsoft.graph.message","id":"AAMk-new","subject":"New mail","isRead":false,"receivedDateTime":"2026-07-17T08:00:00Z"}
				],
				"@odata.deltaLink":"`+srv.URL+advanced+`"
			}`)
		case strings.HasSuffix(r.URL.Path, "/AAMk-new/$value"):
			w.Header().Set("Content-Type", "message/rfc822")
			_, _ = io.WriteString(w, graphTestMIME("AAMk-new", "New mail"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	client := openGraphTestClient(t, srv, token, mailbox)
	if _, err := client.SelectMailbox("INBOX"); err != nil {
		t.Fatalf("SelectMailbox: %v", err)
	}
	client.(GraphDeltaStore).RestoreGraphDelta(srv.URL + saved)
	messages, err := client.FetchMessages(0, 10)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(messages) != 1 || messages[0].FetchError != nil || !bytesContains(messages[0].Raw, "New mail") {
		t.Fatalf("messages = %#v, want only the created message", messages)
	}
	if graphResumeURL(client.(GraphDeltaStore).GraphDelta()) != srv.URL+advanced {
		t.Fatalf("cursor = %q, want the page deltaLink", client.(GraphDeltaStore).GraphDelta())
	}
	joined := strings.Join(calls, "\n")
	for _, id := range []string{"AAMk-deleted", "AAMk-moved", "AAMk-read"} {
		if strings.Contains(joined, "/messages/"+id+"/$value") {
			t.Fatalf("fetched MIME for non-mail delta entry %s\n%s", id, joined)
		}
	}
	if !strings.Contains(joined, "/messages/AAMk-new/$value") {
		t.Fatalf("created message was not downloaded\n%s", joined)
	}
}

func TestGraphContinuationDoesNotSendBearerToken(t *testing.T) {
	const token = "delegated-graph-token"
	var evilCalls []string
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilCalls = append(evilCalls, r.Header.Get("Authorization")+" "+r.URL.RequestURI())
		http.Error(w, "stolen", http.StatusOK)
	}))
	t.Cleanup(evil.Close)

	var reset bool
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !reset && strings.Contains(r.URL.Path, "/messages/delta") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"value":[],"@odata.nextLink":"`+evil.URL+`/steal"}`)
			return
		}
		if r.URL.Query().Get("$deltatoken") == "expired" {
			w.Header().Set("Location", evil.URL+"/steal")
			w.WriteHeader(http.StatusGone)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, graphDeltaJSON(nil, "", srv.URL+"/users/box/mailFolders/inbox/messages/delta?$deltatoken=fresh"))
	}))
	t.Cleanup(srv.Close)

	box := openGraphTestBox(t, srv, token, "box")
	_, cursor, err := box.Poll(context.Background(), GraphMailboxCursor{}, 2)
	if err == nil {
		t.Fatal("foreign nextLink was accepted")
	}
	if cursor.DeltaLink != "" {
		t.Fatalf("cursor = %q, want the previous link", cursor.DeltaLink)
	}
	if len(evilCalls) != 0 {
		t.Fatalf("bearer token was sent to a foreign continuation: %s", strings.Join(evilCalls, "\n"))
	}

	reset = true
	expired := srv.URL + "/users/box/mailFolders/inbox/messages/delta?$deltatoken=expired"
	_, cursor, err = box.Poll(context.Background(), GraphMailboxCursor{DeltaLink: expired}, 2)
	if err != nil {
		t.Fatalf("sync reset: %v", err)
	}
	if strings.Contains(cursor.DeltaLink, evil.URL) || !strings.Contains(cursor.DeltaLink, "deltatoken=fresh") {
		t.Fatalf("cursor after reset = %q", cursor.DeltaLink)
	}
	if len(evilCalls) != 0 {
		t.Fatalf("sync-reset Location received the bearer token: %s", strings.Join(evilCalls, "\n"))
	}
}

func TestGraphMIMEIsNotSilentlyTruncated(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/$value") {
			w.Header().Set("Content-Type", "message/rfc822")
			_, _ = io.WriteString(w, strings.Repeat("x", graphMIMEReadLimit+1))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, graphDeltaJSON([]graphDeltaRow{{ID: "AAMk-big", Received: "2026-07-17T08:00:00Z"}}, "", srv.URL+"/users/box/mailFolders/inbox/messages/delta?$deltatoken=saved"))
	}))
	t.Cleanup(srv.Close)

	box := openGraphTestBox(t, srv, "token", "box")
	messages, _, err := box.Poll(context.Background(), GraphMailboxCursor{}, 2)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(messages) != 1 || messages[0].FetchError == nil || len(messages[0].Raw) != 0 {
		t.Fatalf("oversize message = raw %q err %v, want empty raw and FetchError", messages[0].Raw, messages[0].FetchError)
	}
}

func TestGraphPollHonorsSchedulerCancellation(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"value":[],"@odata.deltaLink":"`+r.Host+`/delta"}`)
	}))
	t.Cleanup(srv.Close)
	box := openGraphTestBox(t, srv, "token", "box")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &graphMailboxIMAP{box: box, ctx: ctx}
	if _, err := client.FetchMessages(0, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchMessages err = %v, want context canceled", err)
	}
	if calls != 0 {
		t.Fatalf("canceled poll made %d Graph calls", calls)
	}
	if client.GraphDelta() != "" {
		t.Fatalf("canceled poll advanced cursor to %q", client.GraphDelta())
	}
}

func TestGraphMissingFolderFailsConnectionTest(t *testing.T) {
	var deltaCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/messages/delta") {
			deltaCalls++
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	provider := NewMicrosoftProvider("client", "secret", "tenant", nil)
	provider.graphBaseURL = srv.URL
	provider.httpClient = srv.Client()
	err := provider.TestConnection(context.Background(), &models.ChannelConfig{
		EmailOAuthAccessToken: "not-a-jwt",
		EmailOAuthEmail:       "box@contoso.example",
		EmailMailbox:          "Missing",
	})
	if err == nil || !strings.Contains(err.Error(), `graph mail folder "Missing" was not found`) {
		t.Fatalf("TestConnection = %v", err)
	}
	if deltaCalls != 0 {
		t.Fatalf("missing folder still started a delta poll (%d calls)", deltaCalls)
	}
}

func TestMicrosoftIMAPGrantIsNotPolledAsGraph(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	t.Cleanup(srv.Close)
	provider := NewMicrosoftProvider("client", "secret", "tenant", []string{
		"https://outlook.office365.com/IMAP.AccessAsUser.All",
		"openid",
		"offline_access",
	})
	auth, err := url.Parse(provider.GetOAuthURL("state", "https://app.example/callback"))
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	scope := auth.Query().Get("scope")
	if !strings.Contains(scope, "https://graph.microsoft.com/Mail.ReadWrite") || !strings.Contains(scope, "offline_access") {
		t.Fatalf("scope = %q, want Graph Mail.ReadWrite and offline_access", scope)
	}
	for _, unwanted := range []string{"IMAP.AccessAsUser", "openid", "SMTP.Send"} {
		if strings.Contains(scope, unwanted) {
			t.Fatalf("scope = %q still requests %s", scope, unwanted)
		}
	}

	provider.graphBaseURL = srv.URL
	provider.httpClient = srv.Client()
	_, err = provider.Connect(context.Background(), &models.ChannelConfig{
		EmailOAuthAccessToken: graphTestJWT("https://outlook.office365.com", "IMAP.AccessAsUser.All"),
		EmailOAuthEmail:       "box@contoso.example",
	})
	if err == nil || !strings.Contains(err.Error(), "reauthorize the mailbox for Microsoft Graph Mail.ReadWrite") {
		t.Fatalf("Connect = %v", err)
	}
	if calls != 0 {
		t.Fatalf("Outlook grant was sent to Graph (%d calls)", calls)
	}
}

func TestGraphReadUpdateDoesNotMarkReadAgain(t *testing.T) {
	const id = "AAMk-already-read"
	var calls []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case strings.Contains(r.URL.Path, "/mailFolders/") && !strings.Contains(r.URL.Path, "/messages/"):
			_, _ = io.WriteString(w, `{"id":"inbox"}`)
		case strings.Contains(r.URL.Path, "/messages/delta"):
			_, _ = io.WriteString(w, graphDeltaJSON([]graphDeltaRow{{ID: id, Received: "2026-07-17T08:00:00Z", IsRead: json.RawMessage(`true`)}}, "", srv.URL+"/users/box/mailFolders/inbox/messages/delta?$deltatoken=next"))
		case strings.HasSuffix(r.URL.Path, "/$value"):
			w.Header().Set("Content-Type", "message/rfc822")
			_, _ = io.WriteString(w, graphTestMIME(id, "Already read"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := openGraphTestClient(t, srv, "token", "box")
	if _, err := client.SelectMailbox("INBOX"); err != nil {
		t.Fatalf("SelectMailbox: %v", err)
	}
	messages, err := client.FetchMessages(0, 2)
	if err != nil || len(messages) != 1 {
		t.Fatalf("fetch = (%d, %v)", len(messages), err)
	}
	if err := client.MarkAsRead(messages[0].UID); err != nil {
		t.Fatalf("MarkAsRead: %v", err)
	}
	if strings.Contains(strings.Join(calls, "\n"), "PATCH") {
		t.Fatalf("already-read delta update was patched again\n%s", strings.Join(calls, "\n"))
	}
}

type graphDeltaRow struct {
	ID       string
	Received string
	IsRead   json.RawMessage
}

func graphDeltaJSON(rows []graphDeltaRow, nextLink, deltaLink string) string {
	var b strings.Builder
	b.WriteString(`{"value":[`)
	for i, row := range rows {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%q`, row.ID)
		if row.Received != "" {
			fmt.Fprintf(&b, `,"receivedDateTime":%q`, row.Received)
		}
		if len(row.IsRead) > 0 {
			fmt.Fprintf(&b, `,"isRead":%s`, row.IsRead)
		}
		b.WriteString("}")
	}
	b.WriteString("]")
	if nextLink != "" {
		fmt.Fprintf(&b, `,"@odata.nextLink":%q`, nextLink)
	}
	if deltaLink != "" {
		fmt.Fprintf(&b, `,"@odata.deltaLink":%q`, deltaLink)
	}
	b.WriteString("}")
	return b.String()
}

func graphTestMIME(id, subject string) string {
	return "Date: Fri, 17 Jul 2026 10:00:00 +0000\r\n" +
		"From: Customer <customer@example.com>\r\n" +
		"To: Inbox <inbox@contoso.example>\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-ID: <" + id + "@example.com>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"\r\n" +
		"Body for " + subject + "\r\n"
}

func graphTestJWT(aud, scp string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"aud":%q,"scp":%q}`, aud, scp)))
	return header + "." + payload + ".sig"
}

func openGraphTestBox(t *testing.T, srv *httptest.Server, token, mailbox string) *GraphMailbox {
	t.Helper()
	box, err := OpenGraphMailbox(context.Background(), GraphMailboxConfig{
		BaseURL:    srv.URL,
		Token:      token,
		Mailbox:    mailbox,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("OpenGraphMailbox: %v", err)
	}
	return box
}

func openGraphTestClient(t *testing.T, srv *httptest.Server, token, mailbox string) IMAPClient {
	t.Helper()
	provider := NewMicrosoftProvider("client", "secret", "tenant", nil)
	provider.graphBaseURL = srv.URL
	provider.httpClient = srv.Client()
	client, err := provider.Connect(context.Background(), &models.ChannelConfig{
		EmailOAuthAccessToken: token,
		EmailOAuthEmail:       mailbox,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return client
}

func TestGraphBaselineReadUpdateDoesNotIngestHistoricalMail(t *testing.T) {
	const token = "delegated-graph-token"
	saved := "/users/box/mailFolders/inbox/messages/delta?$deltatoken=baseline"
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	var phase string
	var calls []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if strings.Contains(r.URL.Path, "/mailFolders/") && !strings.Contains(r.URL.Path, "/messages/") {
			_, _ = io.WriteString(w, `{"id":"inbox"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if phase == "baseline" {
			_, _ = io.WriteString(w, graphDeltaJSON([]graphDeltaRow{{
				ID: "AAMk-old", Received: "2020-01-01T00:00:00Z",
			}}, "", srv.URL+saved))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/$value") {
			w.Header().Set("Content-Type", "message/rfc822")
			_, _ = io.WriteString(w, graphTestMIME("new", "New mail"))
			return
		}
		_, _ = io.WriteString(w, `{
			"value":[
				{"id":"AAMk-old","subject":"Old","isRead":true,"receivedDateTime":"2020-01-01T00:00:00Z","sender":{"emailAddress":{"address":"old@contoso.example"}}},
				{"id":"AAMk-new","subject":"New mail","isRead":false,"receivedDateTime":"`+future+`","sender":{"emailAddress":{"address":"new@contoso.example"}}}
			],
			"@odata.deltaLink":"`+srv.URL+saved+`-next"
		}`)
	}))
	t.Cleanup(srv.Close)
	phase = "baseline"
	client := openGraphTestClient(t, srv, token, "box")
	if _, err := client.SelectMailbox("INBOX"); err != nil {
		t.Fatalf("SelectMailbox: %v", err)
	}
	done, err := client.(GraphBaselineSync).EstablishBaseline(context.Background(), 10)
	if err != nil || !done {
		t.Fatalf("baseline = (%v, %v)", done, err)
	}
	for _, call := range calls {
		if strings.HasSuffix(call, "/$value") {
			t.Fatalf("baseline downloaded MIME: %s", call)
		}
	}
	phase = "incremental"
	calls = nil
	messages, err := client.FetchMessages(0, 10)
	if err != nil {
		t.Fatalf("incremental poll: %v", err)
	}
	if len(messages) != 1 || !bytesContains(messages[0].Raw, "New mail") {
		t.Fatalf("messages = %#v, want only mail received after baseline", messages)
	}
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, "AAMk-old") && strings.Contains(joined, "/$value") {
		t.Fatalf("historical read update was downloaded\n%s", joined)
	}
}

func TestGraphFolderChangeDropsPreviousDeltaLink(t *testing.T) {
	const token = "delegated-graph-token"
	inboxLink := "/users/box/mailFolders/inbox/messages/delta?$deltatoken=inbox"
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.RequestURI())
		if strings.Contains(r.URL.Path, "/mailFolders/") && !strings.Contains(r.URL.Path, "/messages/") {
			_, _ = io.WriteString(w, `{"id":"folder"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, graphDeltaJSON(nil, "", srvSafe(r)+"/users/box/mailFolders/archive/messages/delta?$deltatoken=fresh"))
	}))
	t.Cleanup(srv.Close)
	client := openGraphTestClient(t, srv, token, "box")
	if _, err := client.SelectMailbox("Archive"); err != nil {
		t.Fatalf("SelectMailbox: %v", err)
	}
	stale := "ws-graph-v1\nfolder=inbox\n" + srv.URL + inboxLink
	client.(GraphDeltaStore).RestoreGraphDelta(stale)
	if !client.(GraphCursorControl).GraphFolderMismatch() {
		t.Fatal("folder change reused the previous folder checkpoint")
	}
	if _, err := client.FetchMessages(0, 2); err != nil {
		t.Fatalf("poll: %v", err)
	}
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, "deltatoken=inbox") {
		t.Fatalf("poll resumed the previous folder\n%s", joined)
	}
}

func TestGraphPoisonCheckpointKeepsResumeURL(t *testing.T) {
	const token = "delegated-graph-token"
	saved := "/users/box/mailFolders/inbox/messages/delta?$deltatoken=saved"
	next := "/users/box/mailFolders/inbox/messages/delta?$deltatoken=next"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/mailFolders/") && !strings.Contains(r.URL.Path, "/messages/") {
			_, _ = io.WriteString(w, `{"id":"inbox"}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/$value") {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, graphDeltaJSON([]graphDeltaRow{{ID: "AAMk-bad", Received: "2026-07-17T08:00:00Z"}}, "", srv.URL+next))
	}))
	t.Cleanup(srv.Close)
	client := openGraphTestClient(t, srv, token, "box")
	if _, err := client.SelectMailbox("INBOX"); err != nil {
		t.Fatalf("SelectMailbox: %v", err)
	}
	client.(GraphDeltaStore).RestoreGraphDelta(srv.URL + saved)
	if _, err := client.FetchMessages(0, 2); err == nil {
		t.Fatal("expected MIME failure")
	}
	ctrl := client.(GraphCursorControl)
	ctrl.SetGraphPoison("AAMk-bad", 2)
	held := client.(GraphDeltaStore).GraphDelta()
	if graphResumeURL(held) != srv.URL+saved {
		t.Fatalf("poison checkpoint URL = %q, want the previous link", graphResumeURL(held))
	}
	if !strings.Contains(held, "poison=AAMk-bad") || !strings.Contains(held, "poison-count=2") {
		t.Fatalf("checkpoint = %q, want the Graph id", held)
	}
}

func TestGraphFallbackMessageIDFollowsGraphID(t *testing.T) {
	raw := withGraphFallbackMessageID([]byte("Subject: Hi\r\n\r\nbody\r\n"), "AAMk/one")
	other := withGraphFallbackMessageID([]byte("Subject: Hi\r\n\r\nbody\r\n"), "AAMk/two")
	again := withGraphFallbackMessageID([]byte("Subject: Hi\r\n\r\nbody\r\n"), "AAMk/one")
	if bytes.Equal(raw, other) || !bytes.Equal(raw, again) {
		t.Fatalf("fallback ids are not stable per Graph id")
	}
	parsed := NewParser().Parse(&FetchedMessage{UID: 1, Raw: raw})
	if parsed.MessageID == "" || !strings.Contains(parsed.MessageID, "AAMk/one") {
		t.Fatalf("Message-ID = %q", parsed.MessageID)
	}
	kept := withGraphFallbackMessageID([]byte("Message-ID: <real@example>\r\n\r\nbody\r\n"), "AAMk/one")
	if strings.Contains(string(kept), "windshift.invalid") {
		t.Fatal("fallback replaced an existing Message-ID")
	}
}

func srvSafe(r *http.Request) string {
	return "http://" + r.Host
}

func graphResumeURL(link string) string {
	if strings.HasPrefix(link, "ws-graph-v1\n") {
		if i := strings.LastIndex(link, "\n"); i >= 0 {
			return link[i+1:]
		}
	}
	return link
}

func bytesContains(raw []byte, part string) bool {
	return strings.Contains(string(raw), part)
}
