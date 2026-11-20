package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
)

type mockControlServer struct {
	sockPath string
	dir      string

	mu             sync.Mutex
	statusDoc      statusDocument
	statusCode     int
	statusOnGet    func(n int) (statusDocument, int)
	statusGetCalls int
	conns          []connSnapshot
	localStatus    *ipnstate.Status
	pingHandler    func(ip, pingType string, call int) (*ipnstate.PingResult, int)
	pingCalls      int
	loginCalls     int
	onLoginStart   func()
	lastPingIP     string
	lastPingType   string
}

func startMockControlServer(t *testing.T) *mockControlServer {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix domain socket mock server requires Unix")
	}

	dir, err := os.MkdirTemp("/tmp", "tsup-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen(unix, %q): %v", sockPath, err)
	}

	m := &mockControlServer{
		sockPath:   sockPath,
		dir:        dir,
		statusCode: http.StatusOK,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.statusGetCalls++
		call := m.statusGetCalls
		var doc statusDocument
		var code int
		if m.statusOnGet != nil {
			doc, code = m.statusOnGet(call)
		} else {
			doc, code = m.statusDoc, m.statusCode
		}
		m.mu.Unlock()

		if code == 0 {
			code = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(doc)
	})

	mux.HandleFunc("/debug/connections", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		conns := append([]connSnapshot(nil), m.conns...)
		m.mu.Unlock()

		if conns == nil {
			conns = []connSnapshot{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(conns)
	})

	mux.HandleFunc("/localapi/v0/status", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		st := m.localStatus
		m.mu.Unlock()

		if st == nil {
			st = &ipnstate.Status{BackendState: "Running"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})

	mux.HandleFunc("/localapi/v0/ping", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		ip := q.Get("ip")
		pt := q.Get("type")

		m.mu.Lock()
		m.pingCalls++
		call := m.pingCalls
		m.lastPingIP = ip
		m.lastPingType = pt
		handler := m.pingHandler
		m.mu.Unlock()

		if handler == nil {
			http.Error(w, "no ping handler", http.StatusInternalServerError)
			return
		}
		res, code := handler(ip, pt, call)
		if code == 0 {
			code = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if res != nil {
			_ = json.NewEncoder(w).Encode(res)
		}
	})

	mux.HandleFunc("/localapi/v0/login-interactive", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		m.mu.Lock()
		m.loginCalls++
		cb := m.onLoginStart
		m.mu.Unlock()

		if cb != nil {
			cb()
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	return m
}

func TestRunStatusHelpAndInvalidFlags(t *testing.T) {
	cases := []struct {
		name string
		fn   func([]string, *bytes.Buffer, *bytes.Buffer) int
	}{
		{"status", func(a []string, o, e *bytes.Buffer) int { return runStatusIO(a, o, e) }},
		{"peers", func(a []string, o, e *bytes.Buffer) int { return runPeersIO(a, o, e) }},
		{"ping", func(a []string, o, e *bytes.Buffer) int { return runPingIO(a, o, e) }},
		{"login", func(a []string, o, e *bytes.Buffer) int { return runLoginIO(a, o, e) }},
		{"conns", func(a []string, o, e *bytes.Buffer) int { return runConnsIO(a, o, e) }},
	}

	for _, tc := range cases {
		for _, helpFlag := range []string{"-h", "--help"} {
			var out, errBuf bytes.Buffer
			code := tc.fn([]string{helpFlag}, &out, &errBuf)
			if code != 0 {
				t.Errorf("%s %s: exit = %d, want 0 (stderr=%q)", tc.name, helpFlag, code, errBuf.String())
			}
			if out.Len() != 0 {
				t.Errorf("%s %s: expected empty stdout, got %q", tc.name, helpFlag, out.String())
			}
			help := errBuf.String()
			for _, want := range []string{"USAGE", "OPTIONS", "SOCKET RESOLUTION", "EXAMPLES"} {
				if !strings.Contains(help, want) {
					t.Errorf("%s %s: usage missing %q:\n%s", tc.name, helpFlag, want, help)
				}
			}
		}

		var out, errBuf bytes.Buffer
		code := tc.fn([]string{"-nonexistent-flag"}, &out, &errBuf)
		if code != 2 {
			t.Errorf("%s invalid flag: exit = %d, want 2", tc.name, code)
		}
	}
}

func TestRunStatusHumanJSONAndExpiry(t *testing.T) {
	srv := startMockControlServer(t)

	fixedNow := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	origNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() { nowFunc = origNow })

	// 1. Running & serving with normal key expiry (>7d), using -dir resolution.
	srv.mu.Lock()
	srv.statusCode = http.StatusOK
	srv.statusDoc = statusDocument{
		Ready:             true,
		Serving:           true,
		Hostname:          "tsunplug-proxy",
		BackendState:      "Running",
		KeyExpiry:         fixedNow.Add(42 * 24 * time.Hour).Format(time.RFC3339),
		RouteAll:          true,
		ExitNode:          "auto:any",
		Suffixes:          []string{"wild-dojo.ts.net"},
		SOCKS5Addr:        "127.0.0.1:1080",
		HTTPAddr:          "127.0.0.1:8080",
		ControlSock:       srv.sockPath,
		Forwards:          []string{"127.0.0.1:7687=neo4j.wild-dojo.ts.net:7687"},
		TailnetOnly:       false,
		ActiveConnections: 4,
		Version:           "0.2.0",
	}
	srv.mu.Unlock()

	var out, errBuf bytes.Buffer
	code := runStatusIO([]string{"-dir", srv.dir}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("status running exit = %d, want 0 (stderr=%q)", code, errBuf.String())
	}

	var jsonOut, jsonErr bytes.Buffer
	if jcode := runStatusIO([]string{"-json", "-socket", srv.sockPath}, &jsonOut, &jsonErr); jcode != 0 {
		t.Fatalf("status running -json exit = %d, want 0 (stderr=%q)", jcode, jsonErr.String())
	} else {
		var readyDoc statusDocument
		if err := json.Unmarshal(jsonOut.Bytes(), &readyDoc); err != nil {
			t.Fatalf("status running -json invalid JSON: %v", err)
		}
		if !readyDoc.Ready || !readyDoc.Serving || readyDoc.ActiveConnections != 4 {
			t.Errorf("status running -json decoded = %+v", readyDoc)
		}
	}
	got := out.String()
	for _, want := range []string{
		"Running (serving)",
		"tsunplug-proxy",
		"0.2.0",
		"(in 42d)",
		"127.0.0.1:1080",
		"127.0.0.1:8080",
		srv.sockPath,
		"127.0.0.1:7687=neo4j.wild-dojo.ts.net:7687",
		"Tailnet-only:",
		"RouteAll:",
		"auto:any",
		"wild-dojo.ts.net",
		"Active connections:",
		"4",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status human output missing %q:\n%s", want, got)
		}
	}

	// 2. Key expiry soon (<7 days) shows WARNING.
	srv.mu.Lock()
	srv.statusDoc.KeyExpiry = fixedNow.Add(3 * 24 * time.Hour).Format(time.RFC3339)
	srv.mu.Unlock()

	out.Reset()
	errBuf.Reset()
	code = runStatusIO([]string{"-socket", srv.sockPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("status expiry-soon exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "WARNING: expires in 3d") {
		t.Errorf("expected expiry warning, got:\n%s", out.String())
	}

	// 3. Key expired shows EXPIRED.
	srv.mu.Lock()
	srv.statusDoc.KeyExpiry = fixedNow.Add(-2 * 24 * time.Hour).Format(time.RFC3339)
	srv.mu.Unlock()

	out.Reset()
	errBuf.Reset()
	code = runStatusIO([]string{"-socket", srv.sockPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("status expired-key exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "EXPIRED 2d ago") {
		t.Errorf("expected EXPIRED notice, got:\n%s", out.String())
	}

	// 4. NeedsLogin returns exit 1 and prints Auth URL (human & JSON).
	srv.mu.Lock()
	srv.statusCode = http.StatusServiceUnavailable
	srv.statusDoc.Ready = false
	srv.statusDoc.Serving = false
	srv.statusDoc.BackendState = "NeedsLogin"
	srv.statusDoc.AuthURL = "https://login.tailscale.com/a/xyz789"
	srv.mu.Unlock()

	out.Reset()
	errBuf.Reset()
	code = runStatusIO([]string{"-socket", srv.sockPath}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("status NeedsLogin human exit = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "NeedsLogin") || !strings.Contains(out.String(), "https://login.tailscale.com/a/xyz789") {
		t.Errorf("NeedsLogin human output missing state/auth_url:\n%s", out.String())
	}

	out.Reset()
	errBuf.Reset()
	code = runStatusIO([]string{"-json", "-socket", srv.sockPath}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("status NeedsLogin -json exit = %d, want 1", code)
	}
	var decoded statusDocument
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("status -json invalid JSON: %v\n%s", err, out.String())
	}
	if decoded.BackendState != "NeedsLogin" || decoded.AuthURL != "https://login.tailscale.com/a/xyz789" {
		t.Errorf("decoded JSON = %+v", decoded)
	}

	// 5. Unreachable socket exits 1 with stderr message.
	out.Reset()
	errBuf.Reset()
	code = runStatusIO([]string{"-socket", filepath.Join(srv.dir, "missing.sock")}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("status missing socket exit = %d, want 1", code)
	}
	if errBuf.Len() == 0 {
		t.Errorf("expected stderr error for missing socket")
	}

	// 6. Unexpected positional arg exits 2.
	out.Reset()
	errBuf.Reset()
	if code := runStatusIO([]string{"extra"}, &out, &errBuf); code != 2 {
		t.Errorf("status unexpected arg exit = %d, want 2", code)
	}
}

func TestRunPeersHumanOnlineAndJSON(t *testing.T) {
	srv := startMockControlServer(t)

	k1 := key.NewNode().Public()
	k2 := key.NewNode().Public()
	k3 := key.NewNode().Public()
	kSharee := key.NewNode().Public()

	tags := views.SliceOf([]string{"tag:server", "tag:prod"})
	srv.mu.Lock()
	srv.localStatus = &ipnstate.Status{
		BackendState:   "Running",
		MagicDNSSuffix: "wild-dojo.ts.net",
		Self: &ipnstate.PeerStatus{
			ID:           "self-id",
			HostName:     "tsunplug-proxy",
			DNSName:      "tsunplug-proxy.wild-dojo.ts.net.",
			OS:           "macOS",
			TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.1")},
			Online:       true,
			Relay:        "nyc",
		},
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			k1: {
				ID:             "peer-1",
				HostName:       "zeta",
				DNSName:        "zeta.wild-dojo.ts.net.",
				OS:             "linux",
				TailscaleIPs:   []netip.Addr{netip.MustParseAddr("100.64.0.20")},
				Online:         true,
				Active:         true,
				CurAddr:        "203.0.113.20:41641",
				Relay:          "sfo",
				ExitNode:       true,
				ExitNodeOption: true,
				Tags:           &tags,
			},
			k2: {
				ID:           "peer-2",
				HostName:     "alpha",
				DNSName:      "alpha.wild-dojo.ts.net.",
				OS:           "linux",
				TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.10")},
				Online:       true,
				Relay:        "iad",
			},
			k3: {
				ID:           "peer-3",
				HostName:     "bravo",
				DNSName:      "bravo.wild-dojo.ts.net.",
				OS:           "windows",
				TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.30")},
				Online:       false,
			},
			kSharee: {
				ID:           "peer-sharee",
				HostName:     "hidden-sharee",
				DNSName:      "hidden-sharee.wild-dojo.ts.net.",
				OS:           "linux",
				TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.99")},
				Online:       true,
				ShareeNode:   true,
			},
		},
	}
	srv.mu.Unlock()

	// 1. Default human table shows self first, online peers sorted alphabetically, then offline.
	var out, errBuf bytes.Buffer
	code := runPeersIO([]string{"-socket", srv.sockPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("peers exit = %d, want 0 (stderr=%q)", code, errBuf.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 { // header + self + 3 non-sharee peers
		t.Fatalf("expected 5 lines, got %d:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "IP") || !strings.Contains(lines[0], "NAME") || !strings.Contains(lines[0], "PATH") {
		t.Errorf("unexpected header: %q", lines[0])
	}
	if !strings.Contains(lines[1], "tsunplug-proxy.wild-dojo.ts.net (self)") {
		t.Errorf("line 1 should be self: %q", lines[1])
	}
	if !strings.Contains(lines[2], "alpha.wild-dojo.ts.net") || !strings.Contains(lines[2], "relay (iad)") {
		t.Errorf("line 2 should be online alpha via relay: %q", lines[2])
	}
	if !strings.Contains(lines[3], "zeta.wild-dojo.ts.net") || !strings.Contains(lines[3], "direct (203.0.113.20:41641)") || !strings.Contains(lines[3], "active, exit node") {
		t.Errorf("line 3 should be active zeta direct: %q", lines[3])
	}
	if !strings.Contains(lines[4], "bravo.wild-dojo.ts.net") || !strings.Contains(lines[4], "offline") || !strings.HasSuffix(strings.TrimSpace(lines[4]), "-") {
		t.Errorf("line 4 should be offline bravo: %q", lines[4])
	}
	if strings.Contains(out.String(), "hidden-sharee") {
		t.Errorf("ShareeNode should be hidden:\n%s", out.String())
	}

	// 2. -online filters out offline peers.
	out.Reset()
	errBuf.Reset()
	code = runPeersIO([]string{"-online", "-socket", srv.sockPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("peers -online exit = %d, want 0", code)
	}
	if strings.Contains(out.String(), "bravo") {
		t.Errorf("-online should exclude offline peer bravo:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "alpha") || !strings.Contains(out.String(), "zeta") {
		t.Errorf("-online should include online peers:\n%s", out.String())
	}

	// 3. -json outputs structured summary array.
	out.Reset()
	errBuf.Reset()
	code = runPeersIO([]string{"-json", "-socket", srv.sockPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("peers -json exit = %d, want 0", code)
	}
	var got []peerSummary
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("peers -json invalid JSON: %v\n%s", err, out.String())
	}
	if len(got) != 4 {
		t.Fatalf("got %d peers in JSON, want 4", len(got))
	}
	if !got[0].Self || got[0].Hostname != "tsunplug-proxy" {
		t.Errorf("first JSON peer = %+v, want self", got[0])
	}
	if got[2].Hostname != "zeta" || !got[2].ExitNode || len(got[2].Tags) != 2 || got[2].Tags[0] != "tag:server" {
		t.Errorf("zeta JSON peer = %+v", got[2])
	}
	if got[1].Tags == nil {
		t.Errorf("expected non-nil empty tags slice in JSON")
	}

	// 4. Empty peer list prints helpful message.
	srv.mu.Lock()
	srv.localStatus = &ipnstate.Status{BackendState: "Running"}
	srv.mu.Unlock()
	out.Reset()
	errBuf.Reset()
	code = runPeersIO([]string{"-socket", srv.sockPath}, &out, &errBuf)
	if code != 0 || !strings.Contains(out.String(), "No peers found.") {
		t.Errorf("empty peers: code=%d out=%q", code, out.String())
	}
}

func TestRunPingDirectDERPAndErrors(t *testing.T) {
	srv := startMockControlServer(t)

	origInterval := pingInterval
	pingInterval = 0
	t.Cleanup(func() { pingInterval = origInterval })

	kPeer := key.NewNode().Public()
	srv.mu.Lock()
	srv.localStatus = &ipnstate.Status{
		BackendState:   "Running",
		MagicDNSSuffix: "wild-dojo.ts.net",
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			kPeer: {
				HostName:     "gxa",
				DNSName:      "gxa.wild-dojo.ts.net.",
				TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.42")},
				Online:       true,
			},
		},
	}
	srv.pingHandler = func(ip, pingType string, call int) (*ipnstate.PingResult, int) {
		if call == 1 {
			return &ipnstate.PingResult{
				IP:             ip,
				NodeIP:         ip,
				NodeName:       "gxa.wild-dojo.ts.net.",
				DERPRegionID:   1,
				DERPRegionCode: "nyc",
				LatencySeconds: 0.0456,
			}, http.StatusOK
		}
		return &ipnstate.PingResult{
			IP:             ip,
			NodeIP:         ip,
			NodeName:       "gxa.wild-dojo.ts.net.",
			Endpoint:       "203.0.113.42:41641",
			LatencySeconds: 0.0123,
		}, http.StatusOK
	}
	srv.mu.Unlock()

	// 1. Short MagicDNS name with -c 2: first DERP, second direct.
	var out, errBuf bytes.Buffer
	code := runPingIO([]string{"-c", "2", "-type", "tsmp", "-socket", srv.sockPath, "gxa"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("ping gxa exit = %d, want 0 (stderr=%q)", code, errBuf.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 pong lines, got %d:\n%s", len(lines), out.String())
	}
	if lines[0] != "pong from gxa.wild-dojo.ts.net (100.64.0.42) via DERP(nyc) in 45.6ms" {
		t.Errorf("line 0 = %q", lines[0])
	}
	if lines[1] != "pong from gxa.wild-dojo.ts.net (100.64.0.42) via 203.0.113.42:41641 in 12.3ms" {
		t.Errorf("line 1 = %q", lines[1])
	}

	srv.mu.Lock()
	gotIP, gotType := srv.lastPingIP, srv.lastPingType
	srv.mu.Unlock()
	if gotIP != "100.64.0.42" || gotType != string(tailcfg.PingTSMP) {
		t.Errorf("server got ip=%q type=%q, want 100.64.0.42 TSMP", gotIP, gotType)
	}

	// 2. Direct IP literal target works.
	srv.mu.Lock()
	srv.pingCalls = 0
	srv.mu.Unlock()
	out.Reset()
	errBuf.Reset()
	code = runPingIO([]string{"-c", "1", "-socket", srv.sockPath, "100.64.0.42"}, &out, &errBuf)
	if code != 0 || !strings.Contains(out.String(), "via DERP(nyc)") {
		t.Errorf("ping IP literal: code=%d out=%q err=%q", code, out.String(), errBuf.String())
	}

	// 3. Ping error in PingResult.Err returns exit 1.
	srv.mu.Lock()
	srv.pingHandler = func(ip, pingType string, call int) (*ipnstate.PingResult, int) {
		return &ipnstate.PingResult{
			IP:  ip,
			Err: "no matching peer",
		}, http.StatusOK
	}
	srv.mu.Unlock()
	out.Reset()
	errBuf.Reset()
	code = runPingIO([]string{"-c", "1", "-socket", srv.sockPath, "gxa.wild-dojo.ts.net"}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("ping error exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "no matching peer") {
		t.Errorf("expected ping error on stderr, got %q", errBuf.String())
	}

	// 4. Unknown hostname fails cleanly with exit 1.
	out.Reset()
	errBuf.Reset()
	code = runPingIO([]string{"-c", "1", "-socket", srv.sockPath, "does-not-exist"}, &out, &errBuf)
	if code != 1 || !strings.Contains(errBuf.String(), "not found") {
		t.Errorf("unknown host: code=%d err=%q", code, errBuf.String())
	}

	// 5. Invalid flags/arguments return exit 2.
	for _, badArgs := range [][]string{
		{"-c", "0", "gxa"},
		{"-timeout", "0s", "gxa"},
		{"-type", "bogus", "gxa"},
		{},
		{"a", "b"},
	} {
		out.Reset()
		errBuf.Reset()
		if c := runPingIO(badArgs, &out, &errBuf); c != 2 {
			t.Errorf("ping %v: exit = %d, want 2", badArgs, c)
		}
	}
}

func TestRunLoginTransitionsOpenAndWait(t *testing.T) {
	srv := startMockControlServer(t)

	origPoll := loginPollInterval
	origOpen := openURLFunc
	loginPollInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		loginPollInterval = origPoll
		openURLFunc = origOpen
	})

	// 1. Already logged in returns 0 immediately without calling login-interactive.
	srv.mu.Lock()
	srv.statusCode = http.StatusOK
	srv.statusDoc = statusDocument{
		Ready:        true,
		Serving:      true,
		BackendState: "Running",
		Hostname:     "tsunplug-proxy",
	}
	srv.mu.Unlock()

	var out, errBuf bytes.Buffer
	code := runLoginIO([]string{"-socket", srv.sockPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("login already-running exit = %d, want 0 (stderr=%q)", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "Already logged in (tsunplug-proxy)") {
		t.Errorf("unexpected stdout: %q", out.String())
	}
	srv.mu.Lock()
	if srv.loginCalls != 0 {
		t.Errorf("loginCalls = %d, want 0", srv.loginCalls)
	}
	srv.mu.Unlock()

	// 2. NeedsLogin (no AuthURL initially) -> calls login-interactive -> delivers AuthURL
	//    -> -open calls openURLFunc -> transitions to Running -> exits 0!
	const wantURL = "https://login.tailscale.com/a/interactive123"
	var openedURL string
	openURLFunc = func(u string) error {
		openedURL = u
		return nil
	}

	var loginStarted bool
	var pollAfterURL int
	srv.mu.Lock()
	srv.statusGetCalls = 0
	srv.loginCalls = 0
	srv.onLoginStart = func() {
		srv.mu.Lock()
		loginStarted = true
		srv.mu.Unlock()
	}
	srv.statusOnGet = func(call int) (statusDocument, int) {
		if !loginStarted {
			return statusDocument{
				BackendState: "NeedsLogin",
				Hostname:     "tsunplug-proxy",
			}, http.StatusServiceUnavailable
		}
		pollAfterURL++
		if pollAfterURL >= 3 {
			return statusDocument{
				Ready:        true,
				Serving:      true,
				BackendState: "Running",
				Hostname:     "tsunplug-proxy",
			}, http.StatusOK
		}
		return statusDocument{
			BackendState: "NeedsLogin",
			AuthURL:      wantURL,
			Hostname:     "tsunplug-proxy",
		}, http.StatusServiceUnavailable
	}
	srv.mu.Unlock()

	out.Reset()
	errBuf.Reset()
	code = runLoginIO([]string{"-open", "-wait=true", "-timeout", "2s", "-socket", srv.sockPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("login -open -wait exit = %d, want 0 (stdout=%q stderr=%q)", code, out.String(), errBuf.String())
	}
	if openedURL != wantURL {
		t.Errorf("openedURL = %q, want %q", openedURL, wantURL)
	}
	if !strings.Contains(out.String(), wantURL) {
		t.Errorf("stdout missing auth URL: %q", out.String())
	}
	if !strings.Contains(out.String(), "Logged in (tsunplug-proxy)") {
		t.Errorf("stdout missing Logged in confirmation: %q", out.String())
	}
	srv.mu.Lock()
	if srv.loginCalls != 1 {
		t.Errorf("loginCalls = %d, want 1", srv.loginCalls)
	}
	srv.mu.Unlock()

	// 3. -open failure warns to stderr and still completes waiting.
	openURLFunc = func(u string) error {
		return errors.New("no display available")
	}
	srv.mu.Lock()
	pollAfterURL = 0
	srv.statusOnGet = func(call int) (statusDocument, int) {
		pollAfterURL++
		if pollAfterURL >= 3 {
			return statusDocument{
				Ready:        true,
				Serving:      true,
				BackendState: "Running",
				Hostname:     "tsunplug-proxy",
			}, http.StatusOK
		}
		return statusDocument{
			BackendState: "NeedsLogin",
			AuthURL:      wantURL,
			Hostname:     "tsunplug-proxy",
		}, http.StatusServiceUnavailable
	}
	srv.mu.Unlock()

	out.Reset()
	errBuf.Reset()
	code = runLoginIO([]string{"-open", "-timeout", "2s", "-socket", srv.sockPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("login -open warn exit = %d, want 0 (stderr=%q)", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no display available") {
		t.Errorf("expected open warning on stderr, got %q", errBuf.String())
	}
	if !strings.Contains(out.String(), "Logged in (tsunplug-proxy)") {
		t.Errorf("expected Logged in on stdout, got %q", out.String())
	}

	// 4. -wait=false prints AuthURL and returns 0 without waiting.
	srv.mu.Lock()
	srv.statusOnGet = func(call int) (statusDocument, int) {
		return statusDocument{
			BackendState: "NeedsLogin",
			AuthURL:      wantURL,
			Hostname:     "tsunplug-proxy",
		}, http.StatusServiceUnavailable
	}
	srv.mu.Unlock()

	out.Reset()
	errBuf.Reset()
	code = runLoginIO([]string{"-wait=false", "-socket", srv.sockPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("login -wait=false exit = %d, want 0", code)
	}
	if strings.TrimSpace(out.String()) != wantURL {
		t.Errorf("login -wait=false stdout = %q, want %q", out.String(), wantURL)
	}

	// 5. Timeout while waiting returns exit 1.
	out.Reset()
	errBuf.Reset()
	code = runLoginIO([]string{"-wait=true", "-timeout", "40ms", "-socket", srv.sockPath}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("login timeout exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "timed out") {
		t.Errorf("expected timeout message on stderr, got %q", errBuf.String())
	}
}

func TestRunConnsEmptyActiveAndJSON(t *testing.T) {
	srv := startMockControlServer(t)

	// 1. Empty connections -> human message and JSON [].
	srv.mu.Lock()
	srv.conns = []connSnapshot{}
	srv.mu.Unlock()

	var out, errBuf bytes.Buffer
	code := runConnsIO([]string{"-socket", srv.sockPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("conns empty exit = %d, want 0 (stderr=%q)", code, errBuf.String())
	}
	if strings.TrimSpace(out.String()) != "No active connections." {
		t.Errorf("conns empty stdout = %q, want 'No active connections.'", out.String())
	}

	out.Reset()
	errBuf.Reset()
	code = runConnsIO([]string{"-json", "-socket", srv.sockPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("conns empty -json exit = %d, want 0", code)
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("conns empty -json = %q, want []", out.String())
	}

	// 2. Active connections -> formatted table & JSON.
	started := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	srv.mu.Lock()
	srv.conns = []connSnapshot{
		{
			ID:         1,
			Proto:      "socks5",
			Target:     "gxa.wild-dojo.ts.net:22",
			StartedAt:  started,
			DurationMS: 95_000,  // 1m35s
			BytesIn:    1434,    // 1.4 KiB
			BytesOut:   2411725, // 2.3 MiB
		},
		{
			ID:         2,
			Proto:      "http",
			Target:     "api.wild-dojo.ts.net:443",
			StartedAt:  started,
			DurationMS: 450,
			BytesIn:    512,
			BytesOut:   0,
		},
	}
	srv.mu.Unlock()

	out.Reset()
	errBuf.Reset()
	code = runConnsIO([]string{"-socket", srv.sockPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("conns active exit = %d, want 0", code)
	}
	got := out.String()
	for _, want := range []string{
		"ID", "PROTO", "TARGET", "AGE", "IN", "OUT",
		"socks5", "gxa.wild-dojo.ts.net:22", "1m35s", "1.4 KiB", "2.3 MiB",
		"http", "api.wild-dojo.ts.net:443", "450ms", "512 B", "0 B",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("conns table missing %q:\n%s", want, got)
		}
	}

	out.Reset()
	errBuf.Reset()
	code = runConnsIO([]string{"-json", "-socket", srv.sockPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("conns active -json exit = %d, want 0", code)
	}
	var decoded []connSnapshot
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("conns -json invalid JSON: %v", err)
	}
	if len(decoded) != 2 || decoded[0].ID != 1 || decoded[1].Proto != "http" {
		t.Errorf("decoded conns = %+v", decoded)
	}
}

func TestFormatHelpers(t *testing.T) {
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	expiryCases := []struct {
		raw  string
		want string
	}{
		{now.Add(42 * 24 * time.Hour).Format(time.RFC3339), "2026-05-13T00:00:00Z (in 42d)"},
		{now.Add(3 * 24 * time.Hour).Format(time.RFC3339), "2026-04-04T00:00:00Z (WARNING: expires in 3d)"},
		{now.Add(5 * time.Hour).Format(time.RFC3339), "2026-04-01T05:00:00Z (WARNING: expires in 5h)"},
		{now.Add(-2 * 24 * time.Hour).Format(time.RFC3339), "2026-03-30T00:00:00Z (EXPIRED 2d ago)"},
		{now.Format(time.RFC3339), "2026-04-01T00:00:00Z (EXPIRED)"},
		{"not-a-timestamp", "not-a-timestamp"},
	}
	for _, tc := range expiryCases {
		if got := formatKeyExpiry(tc.raw, now); got != tc.want {
			t.Errorf("formatKeyExpiry(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	byteCases := []struct {
		in   int64
		want string
	}{
		{-5, "0 B"},
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1434, "1.4 KiB"},
		{2411725, "2.3 MiB"},
		{2 * 1024 * 1024 * 1024, "2.0 GiB"},
	}
	for _, tc := range byteCases {
		if got := formatBytes(tc.in); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}

	durCases := []struct {
		in   time.Duration
		want string
	}{
		{-time.Second, "0s"},
		{0, "0s"},
		{450 * time.Millisecond, "450ms"},
		{12 * time.Second, "12s"},
		{2 * time.Minute, "2m"},
		{95 * time.Second, "1m35s"},
		{3 * time.Hour, "3h"},
		{2*time.Hour + 15*time.Minute, "2h15m"},
		{48 * time.Hour, "2d"},
		{52 * time.Hour, "2d4h"},
	}
	for _, tc := range durCases {
		if got := formatDurationShort(tc.in); got != tc.want {
			t.Errorf("formatDurationShort(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}

	for raw, want := range map[string]tailcfg.PingType{
		"disco":   tailcfg.PingDisco,
		"DISCO":   tailcfg.PingDisco,
		"tsmp":    tailcfg.PingTSMP,
		"TSMP":    tailcfg.PingTSMP,
		"icmp":    tailcfg.PingICMP,
		"peerapi": tailcfg.PingPeerAPI,
	} {
		got, err := parsePingType(raw)
		if err != nil || got != want {
			t.Errorf("parsePingType(%q) = (%q, %v), want (%q, nil)", raw, got, err, want)
		}
	}
	if _, err := parsePingType("udp"); err == nil {
		t.Errorf("parsePingType(udp) succeeded, want error")
	}
}
