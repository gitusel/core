package email

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"windshift/internal/utils"
)

const (
	defaultGraphPageSize = 50
	maxGraphPageSize     = 100
	maxGraphJSONBytes    = 4 << 20
	graphMIMEReadLimit   = 20 << 20
)

var (
	errGraphFolderNotFound   = errors.New("mail folder was not found")
	errGraphMessageTooLarge  = errors.New("graph message exceeds the MIME size limit")
	errGraphResponseTooLarge = errors.New("graph response exceeds the size limit")
)

// GraphMailboxConfig opens a mailbox with a delegated Graph token.
type GraphMailboxConfig struct {
	BaseURL    string
	Token      string
	Mailbox    string
	HTTPClient *http.Client
}

// GraphMailboxCursor is the resume URL from the last delta page. It is either
// @odata.nextLink (this round is not finished) or @odata.deltaLink (the round
// is finished). It is not an IMAP UID and not a receivedDateTime watermark.
type GraphMailboxCursor struct {
	DeltaLink string
}

// GraphMailbox reads one mailbox folder through Microsoft Graph.
type GraphMailbox struct {
	baseURL   string
	token     string
	mailbox   string
	folder    string
	http      *http.Client
	idsByUID  map[uint32]string
	readByUID map[uint32]bool
}

// OpenGraphMailbox builds a Graph mailbox reader. It does not call Graph.
func OpenGraphMailbox(_ context.Context, cfg GraphMailboxConfig) (*GraphMailbox, error) {
	token := strings.TrimSpace(cfg.Token)
	mailbox := strings.TrimSpace(cfg.Mailbox)
	if token == "" {
		return nil, fmt.Errorf("graph mailbox token is required")
	}
	if mailbox == "" {
		return nil, fmt.Errorf("graph mailbox is required")
	}
	base, err := parseGraphBase(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	return &GraphMailbox{
		baseURL: base,
		token:   token,
		mailbox: mailbox,
		folder:  "inbox",
		http:    graphHTTPClient(cfg.HTTPClient),
	}, nil
}

func parseGraphBase(raw string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	if base == "" {
		base = "https://graph.microsoft.com/v1.0"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("graph base URL is invalid")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !loopbackHost(u.Hostname()) {
			return "", fmt.Errorf("graph base URL must use https")
		}
	default:
		return "", fmt.Errorf("graph base URL must be http or https")
	}
	return base, nil
}

func loopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

func graphHTTPClient(hc *http.Client) *http.Client {
	if hc == nil {
		hc = utils.NewHTTPClient(30 * time.Second)
	}
	clone := *hc
	// Graph delta links are absolute. A 3xx must not be followed: the default
	// client would send the bearer token to a same-host redirect target we
	// have not allowlisted.
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
}

// SelectFolder maps a scheduler folder name onto a Graph mailFolders segment.
// INBOX, Inbox, and inbox all mean the well-known folder inbox. It does not
// call Graph. The scheduler path validates the folder in SelectMailbox.
func (b *GraphMailbox) SelectFolder(name string) error {
	if seg, ok := graphWellKnownFolder(name); ok {
		b.folder = seg
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		b.folder = "inbox"
		return nil
	}
	b.folder = name
	return nil
}

// bindFolder validates the folder and stores the segment used in delta URLs.
func (b *GraphMailbox) bindFolder(ctx context.Context, name string) error {
	segment, err := b.resolveFolder(ctx, name)
	if err != nil {
		return err
	}
	b.folder = segment
	return nil
}

func graphWellKnownFolder(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "inbox":
		return "inbox", true
	case "sent", "sent items", "sentitems", "sent mail":
		return "sentitems", true
	case "drafts", "draft":
		return "drafts", true
	case "junk", "junk email", "junkemail", "spam":
		return "junkemail", true
	case "deleted", "deleted items", "deleteditems", "trash":
		return "deleteditems", true
	case "archive", "archived":
		return "archive", true
	case "outbox":
		return "outbox", true
	default:
		return "", false
	}
}

func splitFolderPath(name string) []string {
	name = strings.ReplaceAll(name, "\\", "/")
	var parts []string
	for _, part := range strings.Split(name, "/") {
		part = strings.TrimSpace(part)
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func (b *GraphMailbox) resolveFolder(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "INBOX"
	}
	parts := splitFolderPath(name)
	if len(parts) == 0 {
		return "", fmt.Errorf("graph mail folder %q was not found", name)
	}
	if len(parts) == 1 {
		if seg, ok := graphWellKnownFolder(parts[0]); ok {
			if err := b.probeFolder(ctx, seg); err != nil {
				if errors.Is(err, errGraphFolderNotFound) {
					return "", fmt.Errorf("graph mail folder %q was not found", name)
				}
				return "", err
			}
			return seg, nil
		}
		if err := b.probeFolder(ctx, parts[0]); err == nil {
			return parts[0], nil
		} else if !errors.Is(err, errGraphFolderNotFound) {
			return "", err
		}
		id, err := b.folderIDByDisplayName(ctx, "", parts[0])
		if err != nil {
			if errors.Is(err, errGraphFolderNotFound) {
				return "", fmt.Errorf("graph mail folder %q was not found", name)
			}
			return "", err
		}
		return id, nil
	}
	parent := ""
	for i, part := range parts {
		if i == 0 {
			if seg, ok := graphWellKnownFolder(part); ok {
				if err := b.probeFolder(ctx, seg); err != nil {
					if errors.Is(err, errGraphFolderNotFound) {
						return "", fmt.Errorf("graph mail folder %q was not found", name)
					}
					return "", err
				}
				parent = seg
				continue
			}
		}
		id, err := b.folderIDByDisplayName(ctx, parent, part)
		if err != nil {
			if errors.Is(err, errGraphFolderNotFound) {
				return "", fmt.Errorf("graph mail folder %q was not found", name)
			}
			return "", err
		}
		parent = id
	}
	return parent, nil
}

func (b *GraphMailbox) probeFolder(ctx context.Context, segment string) error {
	rawURL := fmt.Sprintf("%s/users/%s/mailFolders/%s?$select=id", b.baseURL, url.PathEscape(b.mailbox), url.PathEscape(segment))
	_, err := b.getJSON(ctx, rawURL, 0)
	if err == nil {
		return nil
	}
	var status *graphStatusError
	if errors.As(err, &status) && status.Status == http.StatusNotFound {
		return errGraphFolderNotFound
	}
	return err
}

func (b *GraphMailbox) folderIDByDisplayName(ctx context.Context, parentID, display string) (string, error) {
	q := url.Values{}
	q.Set("$filter", "displayName eq '"+strings.ReplaceAll(display, "'", "''")+"'")
	q.Set("$select", "id,displayName")
	q.Set("$top", "5")
	var rawURL string
	if parentID == "" {
		rawURL = fmt.Sprintf("%s/users/%s/mailFolders?%s", b.baseURL, url.PathEscape(b.mailbox), q.Encode())
	} else {
		rawURL = fmt.Sprintf("%s/users/%s/mailFolders/%s/childFolders?%s", b.baseURL, url.PathEscape(b.mailbox), url.PathEscape(parentID), q.Encode())
	}
	body, err := b.getJSON(ctx, rawURL, 0)
	if err != nil {
		var status *graphStatusError
		if errors.As(err, &status) && status.Status == http.StatusNotFound {
			return "", errGraphFolderNotFound
		}
		return "", err
	}
	var page struct {
		Value []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return "", fmt.Errorf("graph folder decode: %w", err)
	}
	for _, folder := range page.Value {
		if strings.EqualFold(folder.DisplayName, display) && folder.ID != "" {
			return folder.ID, nil
		}
	}
	return "", errGraphFolderNotFound
}

type graphDeltaItem struct {
	ID                string          `json:"id"`
	IsRead            json.RawMessage `json:"isRead"`
	Subject           string          `json:"subject"`
	ReceivedDateTime  string          `json:"receivedDateTime"`
	InternetMessageID string          `json:"internetMessageId"`
	BodyPreview       string          `json:"bodyPreview"`
	Sender            json.RawMessage `json:"sender"`
	From              json.RawMessage `json:"from"`
	Removed           json.RawMessage `json:"@removed"`
}

func (item graphDeltaItem) removed() bool {
	raw := bytes.TrimSpace(item.Removed)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

func (item graphDeltaItem) readKnown() (bool, bool) {
	raw := bytes.TrimSpace(item.IsRead)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false, false
	}
	var flag bool
	if json.Unmarshal(raw, &flag) == nil {
		return true, flag
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return true, strings.EqualFold(text, "true")
	}
	return false, false
}

// ingest reports whether this delta entry is a message to download. Graph
// returns @removed tombstones for deletes and moves, and read/unread notices,
// even when the initial request asked for changeType=created. Those entries
// are not new mail. A created message selected with receivedDateTime carries
// at least one content property; a sparse id+isRead notice does not.
func (item graphDeltaItem) ingest() bool {
	if item.ID == "" || item.removed() {
		return false
	}
	if strings.TrimSpace(item.ReceivedDateTime) != "" || strings.TrimSpace(item.Subject) != "" ||
		strings.TrimSpace(item.InternetMessageID) != "" || strings.TrimSpace(item.BodyPreview) != "" ||
		jsonPresent(item.Sender) || jsonPresent(item.From) {
		return true
	}
	return false
}

func (item graphDeltaItem) arrivedAfter(notBefore time.Time) bool {
	if notBefore.IsZero() {
		return true
	}
	received := strings.TrimSpace(item.ReceivedDateTime)
	if received == "" {
		// A baseline cutoff is set only after declining to import existing mail.
		// An undated incremental entry is a read/unread or property notice.
		// Created messages carry the selected receivedDateTime.
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, received)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, received)
		if err != nil {
			return false
		}
	}
	return !parsed.Before(notBefore)
}

func jsonPresent(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

type graphDeltaPage struct {
	Value     []graphDeltaItem `json:"value"`
	NextLink  string           `json:"@odata.nextLink"`
	DeltaLink string           `json:"@odata.deltaLink"`
}

// Poll reads one delta page and downloads MIME for messages that are actually
// new. pageSize bounds that page. The returned cursor is the page resume URL
// and must be persisted only after every returned message has a durable
// outcome. On error the previous cursor is unchanged; callers must not store
// an empty link over a saved one.
func (b *GraphMailbox) Poll(ctx context.Context, cursor GraphMailboxCursor, pageSize int) ([]*FetchedMessage, GraphMailboxCursor, error) {
	return b.poll(ctx, cursor, pageSize, time.Time{}, "")
}

func (b *GraphMailbox) poll(ctx context.Context, cursor GraphMailboxCursor, pageSize int, notBefore time.Time, omitID string) ([]*FetchedMessage, GraphMailboxCursor, error) {
	items, resume, _, err := b.deltaPage(ctx, cursor, pageSize)
	if err != nil {
		return nil, cursor, err
	}
	messages := make([]*FetchedMessage, 0, len(items))
	ids := make(map[uint32]string, len(items))
	read := make(map[uint32]bool, len(items))
	for _, item := range items {
		if item.ID == omitID || !item.ingest() || !item.arrivedAfter(notBefore) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, cursor, err
		}
		// UIDs are per poll only. Mail without a Message-ID is deduped by a
		// Graph-ID fallback header, not this number. A stable hash would make
		// a later page's different message look like one already ingested.
		uid := uint32(len(messages) + 1)
		raw, err := b.getMIME(ctx, item.ID)
		if errors.Is(err, errGraphMessageTooLarge) {
			messages = append(messages, &FetchedMessage{
				UID:        uid,
				FetchError: fmt.Errorf("graph message exceeds %d-byte limit", graphMIMEReadLimit),
			})
			ids[uid] = item.ID
			continue
		}
		if err != nil {
			if graphMessageGone(err) {
				slog.Debug("skipping graph message that is no longer in the folder", "id", item.ID)
				continue
			}
			return nil, cursor, err
		}
		messages = append(messages, &FetchedMessage{UID: uid, Raw: withGraphFallbackMessageID(raw, item.ID)})
		ids[uid] = item.ID
		if known, isRead := item.readKnown(); known && isRead {
			read[uid] = true
		}
	}
	b.idsByUID = ids
	b.readByUID = read
	return messages, GraphMailboxCursor{DeltaLink: resume}, nil
}

// baselinePage advances one delta page without downloading MIME. done is true
// when the page carried @odata.deltaLink, so the folder's current state has
// been acknowledged and later polls see only changes.
func (b *GraphMailbox) baselinePage(ctx context.Context, cursor GraphMailboxCursor, pageSize int) (GraphMailboxCursor, bool, error) {
	_, resume, done, err := b.deltaPage(ctx, cursor, pageSize)
	if err != nil {
		return cursor, false, err
	}
	return GraphMailboxCursor{DeltaLink: resume}, done, nil
}

func (b *GraphMailbox) deltaPage(ctx context.Context, cursor GraphMailboxCursor, pageSize int) ([]graphDeltaItem, string, bool, error) {
	pageSize = graphPageSize(pageSize)
	requestURL := strings.TrimSpace(cursor.DeltaLink)
	initial := requestURL == ""
	if initial {
		requestURL = b.initialDeltaURL(pageSize)
	} else {
		var err error
		requestURL, err = b.continuationURL(requestURL)
		if err != nil {
			return nil, "", false, err
		}
	}
	body, err := b.getJSON(ctx, requestURL, pageSize)
	if err != nil && !initial && graphSyncReset(err) {
		slog.Warn("graph delta token expired; restarting folder sync", "folder", b.folder)
		body, err = b.getJSON(ctx, b.initialDeltaURL(pageSize), pageSize)
	}
	if err != nil {
		return nil, "", false, err
	}
	var page graphDeltaPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, "", false, fmt.Errorf("graph delta decode: %w", err)
	}
	resume, done, err := b.resumeURL(page)
	if err != nil {
		return nil, "", false, err
	}
	return page.Value, resume, done, nil
}

func (b *GraphMailbox) resumeURL(page graphDeltaPage) (string, bool, error) {
	// A page contains nextLink or deltaLink, not both. Prefer nextLink if a
	// proxy sent both so we do not skip unread pages.
	raw := strings.TrimSpace(page.NextLink)
	done := false
	if raw == "" {
		raw = strings.TrimSpace(page.DeltaLink)
		done = raw != ""
	}
	if raw == "" {
		return "", false, fmt.Errorf("graph delta page did not include a resume link")
	}
	resume, err := b.continuationURL(raw)
	if err != nil {
		return "", false, err
	}
	return resume, done, nil
}

func (b *GraphMailbox) initialDeltaURL(pageSize int) string {
	// changeType=created is encoded into the token. Graph still returns
	// @removed deletes/moves and read/unread notices; Poll handles those
	// entries instead of assuming the filter removed them.
	return fmt.Sprintf("%s/users/%s/mailFolders/%s/messages/delta?changeType=created&$top=%d",
		b.baseURL, url.PathEscape(b.mailbox), url.PathEscape(b.folder), pageSize)
}

// continuationURL accepts a Graph-supplied nextLink or deltaLink only when it
// stays on this mailbox's endpoint. The bearer token is never sent to any
// other host, scheme, or credential-bearing URL.
func (b *GraphMailbox) continuationURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	next, err := url.Parse(raw)
	if err != nil || next.Host == "" || next.Scheme == "" {
		return "", fmt.Errorf("graph continuation URL is invalid")
	}
	if next.User != nil {
		return "", fmt.Errorf("graph continuation URL must not carry credentials")
	}
	if next.Fragment != "" {
		return "", fmt.Errorf("graph continuation URL is invalid")
	}
	base, err := url.Parse(b.baseURL)
	if err != nil || base.Host == "" {
		return "", fmt.Errorf("graph base URL is invalid")
	}
	if !sameGraphEndpoint(base, next) {
		return "", fmt.Errorf("graph continuation URL is not on the Graph endpoint")
	}
	if !strings.Contains(strings.ToLower(next.EscapedPath()), "/messages/delta") {
		return "", fmt.Errorf("graph continuation URL is not a message delta link")
	}
	if strings.Contains(strings.ToLower(next.RawQuery), "access_token") {
		return "", fmt.Errorf("graph continuation URL must not contain an access token")
	}
	return raw, nil
}

func sameGraphEndpoint(base, next *url.URL) bool {
	if !strings.EqualFold(base.Scheme, next.Scheme) || !strings.EqualFold(base.Hostname(), next.Hostname()) {
		return false
	}
	return equivalentPort(base) == equivalentPort(next)
}

func equivalentPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(u.Scheme, "http") {
		return "80"
	}
	return ""
}

func graphPageSize(n int) int {
	if n <= 0 {
		return defaultGraphPageSize
	}
	if n > maxGraphPageSize {
		return maxGraphPageSize
	}
	return n
}

// MarkRead marks one Graph message read. id is the Graph message id, not an IMAP UID.
func (b *GraphMailbox) MarkRead(ctx context.Context, id string) error {
	return b.write(ctx, http.MethodPatch, b.messageURL(id), []byte(`{"isRead":true}`))
}

// Delete removes one Graph message. Graph delete is immediate, so no later expunge is required.
func (b *GraphMailbox) Delete(ctx context.Context, id string) error {
	return b.write(ctx, http.MethodDelete, b.messageURL(id), nil)
}

func (b *GraphMailbox) messageURL(id string) string {
	return fmt.Sprintf("%s/users/%s/messages/%s", b.baseURL, url.PathEscape(b.mailbox), url.PathEscape(id))
}

func (b *GraphMailbox) getJSON(ctx context.Context, rawURL string, pageSize int) ([]byte, error) {
	return b.doGET(ctx, rawURL, "application/json", maxGraphJSONBytes, pageSize)
}

func (b *GraphMailbox) getMIME(ctx context.Context, id string) ([]byte, error) {
	rawURL := b.messageURL(id) + "/$value"
	return b.doGET(ctx, rawURL, "message/rfc822", graphMIMEReadLimit, 0)
}

func (b *GraphMailbox) doGET(ctx context.Context, rawURL, accept string, limit int64, pageSize int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("graph mailbox request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Accept", accept)
	if pageSize > 0 && strings.Contains(rawURL, "/messages/delta") {
		req.Header.Set("Prefer", fmt.Sprintf("odata.maxpagesize=%d", pageSize))
	}
	resp, err := b.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("graph mailbox GET: %w", err)
	}
	defer resp.Body.Close()
	path := graphPathForError(rawURL)
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, &graphStatusError{Status: resp.StatusCode, Op: http.MethodGet, Path: path}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &graphStatusError{Status: resp.StatusCode, Op: http.MethodGet, Path: path, Body: readErrorSnippet(resp.Body)}
	}
	body, err := readBounded(resp.Body, limit)
	if err != nil {
		if errors.Is(err, errGraphResponseTooLarge) && accept == "message/rfc822" {
			return nil, errGraphMessageTooLarge
		}
		return nil, fmt.Errorf("graph mailbox read: %w", err)
	}
	return body, nil
}

func (b *GraphMailbox) write(ctx context.Context, method, rawURL string, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return fmt.Errorf("graph mailbox request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("graph mailbox %s: %w", method, err)
	}
	defer resp.Body.Close()
	path := graphPathForError(rawURL)
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return &graphStatusError{Status: resp.StatusCode, Op: method, Path: path}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &graphStatusError{Status: resp.StatusCode, Op: method, Path: path, Body: readErrorSnippet(resp.Body)}
	}
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return err
}

type graphStatusError struct {
	Status int
	Op     string
	Path   string
	Body   string
}

func (e *graphStatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("graph mailbox %s %s: HTTP %d", e.Op, e.Path, e.Status)
	}
	return fmt.Sprintf("graph mailbox %s %s: HTTP %d: %s", e.Op, e.Path, e.Status, e.Body)
}

func graphSyncReset(err error) bool {
	var status *graphStatusError
	if !errors.As(err, &status) {
		return false
	}
	if status.Status == http.StatusGone {
		return true
	}
	return strings.Contains(strings.ToLower(status.Body), "syncstatenotfound")
}

func graphMessageGone(err error) bool {
	var status *graphStatusError
	if !errors.As(err, &status) {
		return false
	}
	return status.Status == http.StatusNotFound || status.Status == http.StatusGone
}

func graphPathForError(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "graph"
	}
	return u.Path
}

func readErrorSnippet(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, 4096))
	s := strings.TrimSpace(string(data))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	lower := strings.ToLower(s)
	if strings.Contains(lower, "access_token") || strings.Contains(s, "Bearer ") {
		return "response redacted"
	}
	return s
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	if limit < 0 {
		limit = 0
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errGraphResponseTooLarge
	}
	return data, nil
}

// graphMailboxIMAP lets the existing poller consume Graph mail without dialing IMAP.
type graphMailboxIMAP struct {
	box            *GraphMailbox
	ctx            context.Context
	folder         string
	cursor         GraphMailboxCursor
	checkpoint     graphCheckpoint
	pendingURL     string
	holdResume     bool
	folderMismatch bool
	omitPoison     string
	ids            map[uint32]string
	alreadyRead    map[uint32]bool
}

func (g *graphMailboxIMAP) opCtx() context.Context {
	if g.ctx != nil {
		return g.ctx
	}
	return context.Background()
}

func (g *graphMailboxIMAP) SelectMailbox(name string) (*imap.SelectData, error) {
	g.cursor = GraphMailboxCursor{}
	g.pendingURL = ""
	g.holdResume = false
	g.omitPoison = ""
	g.ids = nil
	g.alreadyRead = nil
	if err := g.box.bindFolder(g.opCtx(), name); err != nil {
		return nil, err
	}
	g.folder = g.box.folder
	return &imap.SelectData{UIDValidity: 1, UIDNext: 1}, nil
}

func (g *graphMailboxIMAP) RestoreGraphDelta(link string) {
	cp := parseGraphCheckpoint(link)
	g.folderMismatch = cp.Folder != "" && g.folder != "" && cp.Folder != g.folder
	g.pendingURL = ""
	g.holdResume = false
	g.omitPoison = ""
	if g.folderMismatch {
		g.checkpoint = graphCheckpoint{Folder: g.folder}
		g.cursor = GraphMailboxCursor{}
		return
	}
	if cp.Folder == "" {
		cp.Folder = g.folder
	}
	g.checkpoint = cp
	g.cursor = GraphMailboxCursor{DeltaLink: cp.URL}
}

func (g *graphMailboxIMAP) GraphDelta() string {
	cp := g.checkpoint
	cp.Folder = g.folder
	if g.holdResume {
		cp.URL = g.checkpoint.URL
	} else if g.pendingURL != "" {
		cp.URL = g.pendingURL
		cp.PoisonID = ""
		cp.PoisonCount = 0
	} else {
		cp.URL = g.cursor.DeltaLink
	}
	if strings.TrimSpace(cp.URL) == "" {
		return ""
	}
	return encodeGraphCheckpoint(cp)
}

func (g *graphMailboxIMAP) GraphFolderMismatch() bool { return g.folderMismatch }

func (g *graphMailboxIMAP) GraphMessageID(uid uint32) (string, bool) {
	id := g.ids[uid]
	return id, id != ""
}

func (g *graphMailboxIMAP) Poison() (string, int, bool) {
	if g.checkpoint.PoisonID == "" {
		return "", 0, false
	}
	return g.checkpoint.PoisonID, g.checkpoint.PoisonCount, true
}

func (g *graphMailboxIMAP) SetGraphPoison(id string, count int) {
	g.checkpoint.PoisonID = id
	g.checkpoint.PoisonCount = count
	g.holdResume = true
}

func (g *graphMailboxIMAP) OmitExhaustedPoison(max int) {
	if max > 0 && g.checkpoint.PoisonCount >= max && g.checkpoint.PoisonID != "" {
		g.omitPoison = g.checkpoint.PoisonID
	}
}

func (g *graphMailboxIMAP) HoldGraphResume() { g.holdResume = true }

func (g *graphMailboxIMAP) AcceptGraphPage() {
	g.holdResume = false
	if g.pendingURL != "" {
		g.cursor.DeltaLink = g.pendingURL
		g.checkpoint.URL = g.pendingURL
	}
	g.checkpoint.PoisonID = ""
	g.checkpoint.PoisonCount = 0
	g.omitPoison = ""
}

func (g *graphMailboxIMAP) EstablishBaseline(ctx context.Context, pageSize int) (bool, error) {
	if ctx == nil {
		ctx = g.opCtx()
	}
	if g.checkpoint.NotBefore.IsZero() {
		g.checkpoint.NotBefore = time.Now().UTC()
	}
	cursor, done, err := g.box.baselinePage(ctx, g.cursor, pageSize)
	if err != nil {
		return false, err
	}
	g.cursor = cursor
	g.pendingURL = cursor.DeltaLink
	g.checkpoint.URL = cursor.DeltaLink
	g.checkpoint.Folder = g.folder
	g.holdResume = false
	return done, nil
}

func (g *graphMailboxIMAP) FetchMessages(_ uint32, batchSize int) ([]*FetchedMessage, error) {
	messages, cursor, err := g.box.poll(g.opCtx(), g.cursor, batchSize, g.checkpoint.NotBefore, g.omitPoison)
	if err != nil {
		return nil, err
	}
	g.pendingURL = cursor.DeltaLink
	g.cursor = cursor
	g.ids = g.box.idsByUID
	g.alreadyRead = g.box.readByUID
	return messages, nil
}

func (g *graphMailboxIMAP) graphID(uid uint32) (string, error) {
	id := g.ids[uid]
	if id == "" {
		return "", fmt.Errorf("graph message for uid %d is not in the last poll", uid)
	}
	return id, nil
}

func (g *graphMailboxIMAP) MarkAsRead(uid uint32) error {
	if g.alreadyRead[uid] {
		return nil
	}
	id, err := g.graphID(uid)
	if err != nil {
		return err
	}
	return g.box.MarkRead(g.opCtx(), id)
}

func (g *graphMailboxIMAP) DeleteMessage(uid uint32) error {
	id, err := g.graphID(uid)
	if err != nil {
		return err
	}
	return g.box.Delete(g.opCtx(), id)
}

func (g *graphMailboxIMAP) Expunge() error { return nil }
func (g *graphMailboxIMAP) Close() error   { return nil }

const graphCheckpointVersion = "ws-graph-v1"

type graphCheckpoint struct {
	Folder      string
	NotBefore   time.Time
	PoisonID    string
	PoisonCount int
	URL         string
}

func encodeGraphCheckpoint(cp graphCheckpoint) string {
	var b strings.Builder
	b.WriteString(graphCheckpointVersion)
	b.WriteByte('\n')
	if cp.Folder != "" {
		b.WriteString("folder=")
		b.WriteString(cp.Folder)
		b.WriteByte('\n')
	}
	if !cp.NotBefore.IsZero() {
		b.WriteString("not-before=")
		b.WriteString(cp.NotBefore.UTC().Format(time.RFC3339Nano))
		b.WriteByte('\n')
	}
	if cp.PoisonID != "" {
		b.WriteString("poison=")
		b.WriteString(strings.ReplaceAll(cp.PoisonID, "\n", ""))
		b.WriteByte('\n')
		fmt.Fprintf(&b, "poison-count=%d\n", cp.PoisonCount)
	}
	b.WriteString(strings.TrimSpace(cp.URL))
	return b.String()
}

func parseGraphCheckpoint(raw string) graphCheckpoint {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, graphCheckpointVersion+"\n") {
		return graphCheckpoint{URL: raw}
	}
	var cp graphCheckpoint
	for _, line := range strings.Split(raw, "\n")[1:] {
		switch {
		case strings.HasPrefix(line, "folder="):
			cp.Folder = strings.TrimPrefix(line, "folder=")
		case strings.HasPrefix(line, "not-before="):
			if t, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(line, "not-before=")); err == nil {
				cp.NotBefore = t
			}
		case strings.HasPrefix(line, "poison="):
			cp.PoisonID = strings.TrimPrefix(line, "poison=")
		case strings.HasPrefix(line, "poison-count="):
			_, _ = fmt.Sscanf(strings.TrimPrefix(line, "poison-count="), "%d", &cp.PoisonCount)
		case strings.Contains(line, "://"):
			cp.URL = line
		}
	}
	return cp
}

func withGraphFallbackMessageID(raw []byte, graphID string) []byte {
	if len(raw) == 0 || bytes.Contains(bytes.ToLower(raw), []byte("message-id:")) {
		return raw
	}
	id := graphFallbackMessageID(graphID)
	if id == "" {
		return raw
	}
	return append([]byte("Message-ID: "+id+"\r\n"), raw...)
}

func graphFallbackMessageID(graphID string) string {
	if graphID == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("<graph-")
	for _, r := range graphID {
		if r <= ' ' || r == '<' || r == '>' {
			b.WriteByte('-')
			continue
		}
		b.WriteRune(r)
	}
	b.WriteString("@windshift.invalid>")
	return b.String()
}
