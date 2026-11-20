package main

import (
	"io"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"sync"
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

func TestPACBaselineEmptySuffixesFailClosed(t *testing.T) {
	cfg := pacConfig{
		suffixes: nil,
		proxies:  []string{"PROXY 127.0.0.1:8080"},
	}
	if got := cfg.effectiveSuffixes(); !slices.Equal(got, []string{"ts.net"}) {
		t.Fatalf("effectiveSuffixes = %v, want [ts.net]", got)
	}
	for _, host := range []string{"foo.ts.net", "gxa.wild-dojo.ts.net", "100.64.1.2", "fd7a:115c:a1e0::1"} {
		if got := kind(cfg.match(host)); got != "proxy" {
			t.Errorf("baseline match(%q) = %s, want proxy", host, got)
		}
	}
	for _, host := range []string{"example.com", "10.0.0.1"} {
		if got := kind(cfg.match(host)); got != "direct" {
			t.Errorf("baseline match(%q) = %s, want direct", host, got)
		}
	}
}

func TestMergeSuffixes(t *testing.T) {
	got := mergeSuffixes(
		[]string{"corp.internal.", "Wild-Dojo.ts.net"},
		[]string{"wild-dojo.ts.net.", "extra.ts.net", "  "},
	)
	want := []string{"corp.internal", "wild-dojo.ts.net", "extra.ts.net"}
	if !slices.Equal(got, want) {
		t.Fatalf("mergeSuffixes = %v, want %v", got, want)
	}
}

func TestPACHolderAtomicSwapAndLiveServer(t *testing.T) {
	proxies := []string{"PROXY 127.0.0.1:8080"}
	holder := newPACHolder(pacConfig{suffixes: nil, proxies: proxies})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	status := &statusState{hostname: "pac-test", proxies: proxies}
	serveErr := make(chan serveResult, 1)
	startPACServer(ln, holder, status, true, false, serveErr)

	fetchPAC := func() string {
		t.Helper()
		resp, err := http.Get("http://" + ln.Addr().String() + "/proxy.pac")
		if err != nil {
			t.Fatalf("GET /proxy.pac: %v", err)
		}
		defer resp.Body.Close()
		bs, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /proxy.pac: %v", err)
		}
		return string(bs)
	}

	before := fetchPAC()
	if !strings.Contains(before, `"ts.net"`) {
		t.Fatalf("pre-update PAC missing ts.net:\n%s", before)
	}
	if strings.Contains(before, `"corp.example.internal"`) {
		t.Fatalf("pre-update PAC unexpectedly contains corp.example.internal")
	}

	// Concurrently read and update under -race.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_ = holder.Script()
				_ = holder.Config()
			}
		}()
	}
	holder.Update(pacConfig{
		suffixes: []string{"corp.example.internal", "wild-dojo.ts.net"},
		proxies:  proxies,
	})
	wg.Wait()

	after := fetchPAC()
	if !strings.Contains(after, `"corp.example.internal"`) || !strings.Contains(after, `"wild-dojo.ts.net"`) {
		t.Fatalf("post-update PAC missing dynamically discovered suffixes:\n%s", after)
	}
}
