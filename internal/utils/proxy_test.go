package utils

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestProxyFromEnvironmentUnlessBlocked pins the policy gate behind the
// SSRF-safe transports' Proxy hook: HTTP(S)_PROXY must be honored while local
// connections are allowed (the default) and ignored — forcing a direct dial
// through the SafeNetDialer blocklist — after --allow-local-connections=false.
// A leaked proxy in blocked mode would bypass the post-resolution SSRF
// blocklist entirely, so the fail-closed direction is the invariant.
//
// Go caches the proxy environment in a process-wide sync.Once on first
// ProxyFromEnvironment use, so an inherited NO_PROXY (e.g. NO_PROXY='*') or
// an earlier test initializing that cache would poison the assertions. Each
// scenario therefore runs in a subprocess whose proxy environment is fully
// controlled.
func TestProxyFromEnvironmentUnlessBlocked(t *testing.T) {
	// Capture the inherited policy so the parent restore is faithful even if
	// the suite ran with --allow-local-connections=false.
	savedLocal := AllowLocalConnections()
	defer SetAllowLocalConnections(savedLocal)

	if os.Getenv("WINDSHIFT_PROXY_TEST_SCENARIO") != "" {
		runProxyScenario(t, os.Getenv("WINDSHIFT_PROXY_TEST_SCENARIO"),
			os.Getenv("WINDSHIFT_PROXY_TEST_ALLOW_LOCAL"))
		return
	}

	scenarios := []struct {
		name       string
		proxyEnv   [4]string // HTTPS_PROXY, https_proxy, NO_PROXY, no_proxy
		allowLocal string
		wantProxy  string // "" means the helper must return nil
	}{
		{
			name:       "uppercase proxy honored when local connections allowed",
			proxyEnv:   [4]string{"http://proxy.example:3128", "", "", ""},
			allowLocal: "1",
			wantProxy:  "proxy.example:3128",
		},
		{
			name:       "lowercase fallback honored when local connections allowed",
			proxyEnv:   [4]string{"", "http://proxy.example:3128", "", ""},
			allowLocal: "1",
			wantProxy:  "proxy.example:3128",
		},
		{
			name:       "NO_PROXY star suppresses the proxy when allowed",
			proxyEnv:   [4]string{"http://proxy.example:3128", "", "*", "*"},
			allowLocal: "1",
			wantProxy:  "",
		},
		{
			name:       "proxy ignored when local connections blocked",
			proxyEnv:   [4]string{"http://proxy.example:3128", "", "", ""},
			allowLocal: "0",
			wantProxy:  "",
		},
		{
			name:       "proxy nil when no proxy variables set",
			proxyEnv:   [4]string{"", "", "", ""},
			allowLocal: "1",
			wantProxy:  "",
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run", "^TestProxyFromEnvironmentUnlessBlocked$")
			env := append(os.Environ(),
				"WINDSHIFT_PROXY_TEST_SCENARIO="+sc.name,
				"WINDSHIFT_PROXY_TEST_ALLOW_LOCAL="+sc.allowLocal,
			)
			// Strip inherited proxy variables, then set exactly the scenario's.
			for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
				env = append(env, k+"=")
			}
			env = append(env,
				"HTTPS_PROXY="+sc.proxyEnv[0],
				"https_proxy="+sc.proxyEnv[1],
				"NO_PROXY="+sc.proxyEnv[2],
				"no_proxy="+sc.proxyEnv[3],
			)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("subprocess %q failed: %v\n%s", sc.name, err, out)
			}
			if !strings.Contains(string(out), "SCENARIO_OK") {
				t.Fatalf("subprocess did not complete: %s", out)
			}
		})
	}
}

// runProxyScenario executes one scenario inside the re-invoked test binary
// and prints SCENARIO_OK when the helper matched the expectation. The policy
// is set before any proxy call so the process-wide cache is initialized with
// exactly this scenario's environment.
func runProxyScenario(t *testing.T, scenario, allowLocal string) {
	SetAllowLocalConnections(allowLocal == "1")

	req, err := http.NewRequest(http.MethodGet, "https://target.example/x", http.NoBody)
	if err != nil {
		fmt.Println("request error:", err)
		os.Exit(1)
	}

	u, err := ProxyFromEnvironmentUnlessBlocked(req)
	if err != nil {
		fmt.Println("helper error:", err)
		os.Exit(1)
	}

	want := scenarioWant(scenario)
	got := ""
	if u != nil {
		got = u.Host
	}
	if got != want {
		fmt.Printf("scenario %q: want proxy host %q, got %q\n", scenario, want, got)
		os.Exit(1)
	}
	fmt.Println("SCENARIO_OK")
}

func scenarioWant(scenario string) string {
	switch scenario {
	case "uppercase proxy honored when local connections allowed",
		"lowercase fallback honored when local connections allowed":
		return "proxy.example:3128"
	default:
		return ""
	}
}
