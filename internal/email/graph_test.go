package email

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// graphTestServer builds a fake Graph token endpoint matching the v2.0
// client-credentials contract: POST form grant_type=client_credentials,
// scope=https://graph.microsoft.com/.default, JSON access_token + expires_in.
func graphTestServer(t *testing.T, token string, expiresIn int, captured *map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("token endpoint: want POST, got %s", r.Method)
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if captured != nil {
			(*captured)["grant_type"] = r.FormValue("grant_type")
			(*captured)["scope"] = r.FormValue("scope")
			(*captured)["client_id"] = r.FormValue("client_id")
			(*captured)["client_secret"] = r.FormValue("client_secret")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type":   "Bearer",
			"access_token": token,
			"expires_in":   expiresIn,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchGraphTokenSuccess(t *testing.T) {
	captured := map[string]string{}
	srv := graphTestServer(t, "tok-123", 3600, &captured)

	client := NewGraphClient("", srv.URL, http.DefaultClient)
	tok, err := client.FetchToken(context.Background(), "tenant-abc", "cid", "secret")
	if err != nil {
		t.Fatalf("FetchToken: %v", err)
	}
	if tok.AccessToken != "tok-123" {
		t.Fatalf("token = %q", tok.AccessToken)
	}
	if tok.ExpiresAt == nil || time.Until(*tok.ExpiresAt) <= 55*time.Minute {
		t.Fatalf("expiry not ~1h away: %v", tok.ExpiresAt)
	}
	// Client-credentials contract.
	if captured["grant_type"] != "client_credentials" {
		t.Errorf("grant_type = %q", captured["grant_type"])
	}
	if captured["scope"] != "https://graph.microsoft.com/.default" {
		t.Errorf("scope = %q", captured["scope"])
	}
	if captured["client_id"] != "cid" || captured["client_secret"] != "secret" {
		t.Errorf("client creds: %q / %q", captured["client_id"], captured["client_secret"])
	}
	// Tenant must be in the token URL path, on the fake token base.
	if !strings.Contains(client.TokenURL("tenant-abc"), srv.URL+"/tenant-abc/oauth2/v2.0/token") {
		t.Errorf("token URL missing tenant path: %s", client.TokenURL("tenant-abc"))
	}
}

func TestFetchGraphTokenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "invalid_client",
			"error_description": "AADSTS7000215: Invalid client secret",
		})
	}))
	t.Cleanup(srv.Close)

	client := NewGraphClient("", srv.URL, http.DefaultClient)
	_, err := client.FetchToken(context.Background(), "t", "c", "bad")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("error missing AAD code: %v", err)
	}
}

func TestSendMimeViaGraphRequestShape(t *testing.T) {
	// The Graph sendMail MIME contract: POST /users/{upn}/sendMail,
	// Content-Type: text/plain, body = base64(MIME). Assert all three exactly.
	var (
		gotMethod string
		gotPath   string
		gotCT     string
		gotBody   string
		auth      string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		auth = r.Header.Get("Authorization")
		bodyBytes, _ := io.ReadAll(r.Body)
		gotBody = string(bodyBytes)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	mime := []byte("From: windshift@contoso.com\r\nTo: a@b.com\r\nSubject: hi\r\n\r\nhello")
	client := NewGraphClient(srv.URL, "", http.DefaultClient)
	err := client.SendMime(context.Background(), "tok-xyz", "windshift@contoso.com", mime)
	if err != nil {
		t.Fatalf("SendMime: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s", gotMethod)
	}
	if gotPath != "/users/windshift@contoso.com/sendMail" {
		t.Errorf("path = %s", gotPath)
	}
	if gotCT != "text/plain" {
		t.Errorf("Content-Type = %q (Graph MIME format requires text/plain)", gotCT)
	}
	if auth != "Bearer tok-xyz" {
		t.Errorf("Authorization = %q", auth)
	}
	decoded, decErr := base64.StdEncoding.DecodeString(gotBody)
	if decErr != nil {
		t.Fatalf("body is not base64: %v (body=%q)", decErr, gotBody)
	}
	if string(decoded) != string(mime) {
		t.Errorf("decoded MIME mismatch:\n got %q\nwant %q", decoded, mime)
	}
}

func TestSendMimeViaGraphErrorMapping(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantInEr string
	}{
		{"malformed mime", http.StatusBadRequest, `{"error":{"code":"ErrorMimeContentNotFound","message":"Invalid base64 string for MIME content."}}`, "Invalid base64"},
		{"mailbox restricted", http.StatusForbidden, `{"error":{"code":"ErrorAccessDenied","message":"Access is denied."}}`, "403"},
		{"mailbox not found", http.StatusNotFound, `{"error":{"code":"ErrorNonExistentMailbox","message":"no mailbox"}}`, "404"},
		{"throttled", http.StatusTooManyRequests, `{"error":{"code":"ErrorTooManyRequests","message":"throttled"}}`, "429"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			client := NewGraphClient(srv.URL, "", http.DefaultClient)
			err := client.SendMime(context.Background(), "t", "m@x.com", []byte("mime"))
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantInEr) {
				t.Fatalf("error %q missing %q", err.Error(), tt.wantInEr)
			}
		})
	}
}

func TestGraphClientUsesSSRFSafeTransportByDefault(t *testing.T) {
	// NewGraphClient with nil round-tripper must use the shared SSRF-safe
	// outbound client so Graph calls honor the process-wide policy.
	client := NewGraphClient("", "", nil)
	if client.http == nil {
		t.Fatal("nil http client")
	}
	if client.http == http.DefaultClient {
		t.Fatal("must not use http.DefaultClient directly")
	}
	// Default hosts: API on graph.microsoft.com, tokens on the login host —
	// independent hosts per Microsoft identity platform.
	if client.TokenURL("tenant-x") != "https://login.microsoftonline.com/tenant-x/oauth2/v2.0/token" {
		t.Errorf("default token URL = %s", client.TokenURL("tenant-x"))
	}
}

// TestDefaultAPIBaseCarriesVersion pins the Graph API root contract: the
// default apiBase must include the version segment, so SendMime resolves
// https://graph.microsoft.com/v1.0/users/{upn}/sendMail. A bare
// graph.microsoft.com/users/... route returns 404 "Invalid version: users"
// from Microsoft's edge, which only repro'd live — every other test injects
// its own base URL, so this regression could recurred silently.
func TestDefaultAPIBaseCarriesVersion(t *testing.T) {
	if defaultGraphAPIBase != "https://graph.microsoft.com/v1.0" {
		t.Fatalf("defaultGraphAPIBase = %q, want https://graph.microsoft.com/v1.0", defaultGraphAPIBase)
	}
	// The endpoint shape a default-configured client produces.
	client := NewGraphClient("", "https://login.microsoftonline.com", nil)
	reconstructed := fmt.Sprintf("%s/users/%s/sendMail", client.apiBase, "windshift@contoso.com")
	u, err := url.Parse(reconstructed)
	if err != nil {
		t.Fatalf("parse %q: %v", reconstructed, err)
	}
	if u.Path != "/v1.0/users/windshift@contoso.com/sendMail" {
		t.Fatalf("default endpoint resolves %q, want /v1.0/users/.../sendMail", u.Path)
	}
}
