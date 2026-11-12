package main

import (
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
		{"foo.ts.net", "proxy"}, // any ts.net fails closed
		{"funnel-host.ts.net", "proxy"},
		{"100.64.0.0", "proxy"},
		{"100.64.1.1", "proxy"},
		{"100.127.255.255", "proxy"},
		{"100.63.255.255", "direct"},
		{"100.128.0.0", "direct"},
		{"example.com", "direct"},
		{"10.0.0.1", "direct"},
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
