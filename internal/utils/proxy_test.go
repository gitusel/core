package utils

import (
	"net/http"
	"testing"
)

// TestProxyFromEnvironmentUnlessBlocked pins the policy gate behind the
// SSRF-safe transports' Proxy hook: HTTP(S)_PROXY must be honored while local
// connections are allowed (the default) and ignored — forcing a direct dial
// through the SafeNetDialer blocklist — after --allow-local-connections=false.
// A leaked proxy in blocked mode would bypass the post-resolution SSRF
// blocklist entirely, so the fail-closed direction is the invariant.
func TestProxyFromEnvironmentUnlessBlocked(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	t.Cleanup(func() { SetAllowLocalConnections(true) })

	req, err := http.NewRequest(http.MethodGet, "https://target.example/x", http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	// Default state (allowed): proxy env applies.
	SetAllowLocalConnections(true)
	u, err := ProxyFromEnvironmentUnlessBlocked(req)
	if err != nil || u == nil {
		t.Fatalf("allowed mode: want proxy URL, got %v, %v", u, err)
	}
	if u.Host != "proxy.example:3128" {
		t.Fatalf("allowed mode: want proxy.example:3128, got %q", u.Host)
	}

	// Blocked mode: proxy must be nil so the transport dials the target
	// directly and the dialer's blocklist sees the real destination.
	SetAllowLocalConnections(false)
	u, err = ProxyFromEnvironmentUnlessBlocked(req)
	if err != nil || u != nil {
		t.Fatalf("blocked mode: want nil proxy, got %v, %v", u, err)
	}
}