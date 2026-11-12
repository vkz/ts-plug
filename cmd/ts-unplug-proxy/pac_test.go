package main

import (
	"os/exec"
	"strings"
	"testing"
)

func testPAC() pacConfig {
	return pacConfig{
		suffixes: []string{"wild-dojo.ts.net"},
		proxies:  []string{"PROXY 127.0.0.1:8080", "SOCKS5 127.0.0.1:1080"},
	}
}

func kind(decision string) string {
	if decision == "DIRECT" {
		return "direct"
	}
	return "proxy"
}

// TestPACMatchFailClosed verifies the PAC never sends tailnet-named hosts or
// CGNAT literals DIRECT, and only unmatched hosts fall through.
func TestPACMatchFailClosed(t *testing.T) {
	cfg := testPAC()
	cases := []struct {
		host string
		want string
	}{
		{"wild-dojo.ts.net", "proxy"},
		{"gxa.wild-dojo.ts.net", "proxy"},
		{"GXA.WILD-DOJO.TS.NET.", "proxy"},
		{"gxa.wild-dojo.ts.net.", "proxy"}, // trailing dot
		{"foo.ts.net", "proxy"},            // any ts.net fails closed
		{"funnel-host.ts.net", "proxy"},
		{"100.64.0.0", "proxy"},
		{"100.64.1.1", "proxy"},
		{"100.127.255.255", "proxy"},
		{"100.63.255.255", "direct"},
		{"100.128.0.0", "direct"},
		{"example.com", "direct"},
		{"10.0.0.1", "direct"},
		{"fd7a:115c:a1e0::1", "proxy"},   // tailnet IPv6
		{"[fd7a:115c:a1e0::1]", "proxy"}, // bracketed IPv6
		{"fd7a:115c:a1e1::1", "direct"},
		{"2606:4700::1", "direct"},
	}
	for _, tc := range cases {
		if got := kind(cfg.match(tc.host)); got != tc.want {
			t.Errorf("match(%q) = %s, want %s", tc.host, got, tc.want)
		}
	}
}

func TestPACScriptRules(t *testing.T) {
	script := testPAC().script()
	for _, want := range []string{
		`"wild-dojo.ts.net"`,
		`"ts.net"`,
		`isInNet(host, "100.64.0.0", "255.192.0.0")`,
		`isIpLiteral(host)`,
		`normalizeHost`,
		`fd7a:115c:a1e0:`,
		`return tsnetProxies;`,
		`return "DIRECT";`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("PAC script missing %q", want)
		}
	}
	// The suffix/CGNAT checks must come before the DIRECT fallback.
	if strings.Index(script, `return tsnetProxies;`) > strings.Index(script, `return "DIRECT";`) {
		t.Error("PAC script falls through to DIRECT before the tailnet checks")
	}
}

// TestPACScriptWithNode evaluates the generated JavaScript with a real engine
// and standard PAC shims, so a mismatch between the script and the Go mirror is
// caught. It is skipped when node is unavailable.
func TestPACScriptWithNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found; skipping JS PAC evaluation")
	}
	hosts := []string{
		"wild-dojo.ts.net", "gxa.wild-dojo.ts.net", "GXA.WILD-DOJO.TS.NET.",
		"gxa.wild-dojo.ts.net.", "foo.ts.net", "funnel-host.ts.net",
		"100.64.0.0", "100.64.1.1", "100.127.255.255", "100.63.255.255",
		"100.128.0.0", "example.com", "10.0.0.1",
		"fd7a:115c:a1e0::1", "[fd7a:115c:a1e0::1]", "fd7a:115c:a1e1::1", "2606:4700::1",
	}
	harness := `
function dnsDomainIs(host, domain) {
  return host.length > domain.length && host.substring(host.length - domain.length) === domain;
}
function isInNet(ip, pattern, mask) {
  function toInt(s) { var p = s.split("."); return ((+p[0]) << 24) | ((+p[1]) << 16) | ((+p[2]) << 8) | (+p[3]); }
  return (toInt(ip) & toInt(mask)) === (toInt(pattern) & toInt(mask));
}
` + testPAC().script() + `
var hosts = [` + jsStringList(hosts) + `];
hosts.forEach(function(h) { console.log(h + " " + FindProxyForURL("https://" + h + "/", h)); });
`
	out, err := exec.Command(node, "-e", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if parts := strings.SplitN(line, " ", 2); len(parts) == 2 {
			got[parts[0]] = kind(parts[1])
		}
	}
	for _, h := range hosts {
		want := kind(testPAC().match(h))
		if got[h] != want {
			t.Errorf("PAC script %s -> %s, Go mirror says %s", h, got[h], want)
		}
	}
}

func TestPACAdvertiseAddr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1:8080", "127.0.0.1:8080"},
		{"localhost:8080", "localhost:8080"},
		{"[::1]:8080", "[::1]:8080"},
		{"0.0.0.0:8080", "127.0.0.1:8080"},
		{":8080", "127.0.0.1:8080"},
	}
	for _, tc := range cases {
		if got := pacAdvertiseAddr(tc.in); got != tc.want {
			t.Errorf("pacAdvertiseAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsLoopbackListenAddr(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"127.0.0.1:1080", true},
		{"localhost:1080", true},
		{"[::1]:1080", true},
		{"0.0.0.0:1080", false},
		{":1080", false},
		{"10.0.0.1:1080", false},
		{"not-an-addr", false},
	}
	for _, tc := range cases {
		if got := isLoopbackListenAddr(tc.in); got != tc.want {
			t.Errorf("isLoopbackListenAddr(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
