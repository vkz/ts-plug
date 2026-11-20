package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"
)

func shortSockPath(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if len(p) < 95 {
		return p
	}
	alt, err := os.MkdirTemp("/tmp", "tsup-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(alt) })
	return filepath.Join(alt, name)
}

func TestResolveControlSocketPrecedence(t *testing.T) {
	t.Setenv("TS_UNPLUG_PROXY_SOCK", "/env/explicit.sock")
	t.Setenv("TS_UNPLUG_PROXY_DIR", "/env/state-dir")
	t.Setenv("XDG_STATE_HOME", "/env/xdg-state")

	// 1. flagSock wins over everything.
	got, err := resolveControlSocket("/flag/custom.sock", "/flag/dir")
	if err != nil {
		t.Fatalf("case 1: %v", err)
	}
	if got != "/flag/custom.sock" {
		t.Errorf("case 1 = %q, want /flag/custom.sock", got)
	}

	// 2. flagDir beats env vars.
	got, err = resolveControlSocket("", "/flag/dir")
	if err != nil {
		t.Fatalf("case 2: %v", err)
	}
	if want := filepath.Join("/flag/dir", "control.sock"); got != want {
		t.Errorf("case 2 = %q, want %q", got, want)
	}

	// 3. TS_UNPLUG_PROXY_SOCK beats TS_UNPLUG_PROXY_DIR and XDG_STATE_HOME.
	got, err = resolveControlSocket("", "")
	if err != nil {
		t.Fatalf("case 3: %v", err)
	}
	if got != "/env/explicit.sock" {
		t.Errorf("case 3 = %q, want /env/explicit.sock", got)
	}

	// 4. TS_UNPLUG_PROXY_DIR beats XDG_STATE_HOME.
	t.Setenv("TS_UNPLUG_PROXY_SOCK", "")
	got, err = resolveControlSocket("", "")
	if err != nil {
		t.Fatalf("case 4: %v", err)
	}
	if want := filepath.Join("/env/state-dir", "control.sock"); got != want {
		t.Errorf("case 4 = %q, want %q", got, want)
	}

	// 5a. XDG_STATE_HOME default.
	t.Setenv("TS_UNPLUG_PROXY_DIR", "")
	got, err = resolveControlSocket("", "")
	if err != nil {
		t.Fatalf("case 5a: %v", err)
	}
	if want := filepath.Join("/env/xdg-state", "ts-unplug-proxy", "control.sock"); got != want {
		t.Errorf("case 5a = %q, want %q", got, want)
	}

	// 5b. ~/.local/state fallback when XDG_STATE_HOME is empty.
	t.Setenv("XDG_STATE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	got, err = resolveControlSocket("", "")
	if err != nil {
		t.Fatalf("case 5b: %v", err)
	}
	if want := filepath.Join(home, ".local", "state", "ts-unplug-proxy", "control.sock"); got != want {
		t.Errorf("case 5b = %q, want %q", got, want)
	}
}

func TestControlSocketPermissionsStaleAndRefuseNonSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix control socket tests require Unix")
	}

	// Regular file must be refused and NOT deleted.
	regFile := shortSockPath(t, "reg.sock")
	if err := os.WriteFile(regFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenControlSocket(regFile); err == nil {
		t.Fatal("listenControlSocket on regular file succeeded, want error")
	}
	if bs, err := os.ReadFile(regFile); err != nil || string(bs) != "keep" {
		t.Fatalf("regular file was modified or deleted: err=%v data=%q", err, string(bs))
	}

	// Directory must be refused and NOT deleted.
	dirPath := shortSockPath(t, "dir.sock")
	if err := os.Mkdir(dirPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := listenControlSocket(dirPath); err == nil {
		t.Fatal("listenControlSocket on directory succeeded, want error")
	}
	if fi, err := os.Stat(dirPath); err != nil || !fi.IsDir() {
		t.Fatalf("directory was removed: err=%v", err)
	}

	// Create a stale socket left behind on disk (e.g. SIGKILL), verify it is replaced.
	sockPath := shortSockPath(t, "c.sock")
	staleLn, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if ul, ok := staleLn.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = staleLn.Close()

	ln, err := listenControlSocket(sockPath)
	if err != nil {
		t.Fatalf("listenControlSocket over stale socket: %v", err)
	}

	fi, err := os.Lstat(sockPath)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("Lstat control socket: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("mode %v is not a socket", fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket permissions = %#o, want 0600", perm)
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("Close control listener: %v", err)
	}
	if _, err := os.Lstat(sockPath); !os.IsNotExist(err) {
		t.Errorf("control socket still exists after Close: err=%v", err)
	}
}

func TestControlServerStatusConnsAndLocalAPIProxy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix control socket tests require Unix")
	}

	// Mock tsnet LocalAPI server listening on an internal TCP socket, reached
	// through a custom localDial function (just like tsnet's lc.Dial).
	var headerMu sync.Mutex
	var gotHost, gotOrigin, gotReferer string
	mockLocalMux := http.NewServeMux()
	mockLocalMux.HandleFunc("/localapi/v0/status", func(w http.ResponseWriter, r *http.Request) {
		headerMu.Lock()
		gotHost = r.Host
		gotOrigin = r.Header.Get("Origin")
		gotReferer = r.Header.Get("Referer")
		headerMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&ipnstate.Status{
			BackendState: "Running",
			Self:         &ipnstate.PeerStatus{HostName: "mock-node"},
		})
	})
	mockLocalMux.HandleFunc("/localapi/v0/stream-test", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "chunk-1\n")
		flusher.Flush()
		// Keep stream open until client disconnects so we prove FlushInterval: -1
		// delivered chunk-1 immediately without waiting for EOF.
		<-r.Context().Done()
	})
	mockLocalLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer mockLocalLn.Close()
	mockLocalSrv := &http.Server{Handler: mockLocalMux}
	go func() { _ = mockLocalSrv.Serve(mockLocalLn) }()
	defer mockLocalSrv.Close()

	localDial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", mockLocalLn.Addr().String())
	}

	sockPath := shortSockPath(t, "ctrl.sock")
	controlLn, err := listenControlSocket(sockPath)
	if err != nil {
		t.Fatalf("listenControlSocket: %v", err)
	}
	defer controlLn.Close()

	tracker := newConnTracker()
	c, peer := net.Pipe()
	defer peer.Close()
	tc := tracker.Wrap("http", "gxa.wild-dojo.ts.net:443", c)
	defer tc.Close()

	status := &statusState{
		tracker:     tracker,
		hostname:    "test-proxy",
		controlSock: sockPath,
		socks5Addr:  "127.0.0.1:1080",
		statusFn: func(context.Context) (*ipnstate.Status, error) {
			return &ipnstate.Status{
				BackendState: "NeedsLogin",
				AuthURL:      "https://login.tailscale.com/a/abc123",
				Self:         &ipnstate.PeerStatus{HostName: "test-proxy"},
			}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan serveResult, 1)
	startControlServer(ctx, controlLn, localDial, status, tracker, serveErr)

	// 1. fetchControlStatus works during NeedsLogin (HTTP 503) and returns auth_url.
	doc, err := fetchControlStatus(ctx, sockPath)
	if err != nil {
		t.Fatalf("fetchControlStatus: %v", err)
	}
	if doc.BackendState != "NeedsLogin" || doc.AuthURL != "https://login.tailscale.com/a/abc123" {
		t.Errorf("status = %+v, want NeedsLogin with auth_url", doc)
	}
	if doc.ActiveConnections != 1 {
		t.Errorf("active_connections = %d, want 1", doc.ActiveConnections)
	}
	if doc.ControlSock != sockPath {
		t.Errorf("control_sock = %q, want %q", doc.ControlSock, sockPath)
	}

	// 2. /debug/connections returns active tracked connections as JSON array.
	hc := controlHTTPClient(sockPath)
	defer hc.CloseIdleConnections()
	resp, err := hc.Get(controlBaseURL + "/debug/connections")
	if err != nil {
		t.Fatalf("GET /debug/connections: %v", err)
	}
	var conns []connSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&conns); err != nil {
		resp.Body.Close()
		t.Fatalf("decode /debug/connections: %v", err)
	}
	resp.Body.Close()
	if len(conns) != 1 || conns[0].Proto != "http" || conns[0].Target != "gxa.wild-dojo.ts.net:443" {
		t.Fatalf("connections = %+v, want 1 http connection", conns)
	}

	// 3. controlLocalClient talks through /localapi/ reverse proxy.
	lc := controlLocalClient(sockPath)
	st, err := lc.Status(ctx)
	if err != nil {
		t.Fatalf("controlLocalClient.Status: %v", err)
	}
	if st.BackendState != "Running" || st.Self == nil || st.Self.HostName != "mock-node" {
		t.Errorf("proxied localapi status = %+v", st)
	}
	headerMu.Lock()
	upstreamHost := gotHost
	headerMu.Unlock()
	if upstreamHost != "local-tailscaled.sock" {
		t.Errorf("upstream Host = %q, want local-tailscaled.sock", upstreamHost)
	}

	// 4. Verify Origin and Referer are stripped by the reverse proxy Director.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, controlBaseURL+"/localapi/v0/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Referer", "https://evil.example/page")
	resp2, err := hc.Do(req)
	if err != nil {
		t.Fatalf("localapi with origin/referer: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	headerMu.Lock()
	origin, referer, host := gotOrigin, gotReferer, gotHost
	headerMu.Unlock()
	if host != "local-tailscaled.sock" {
		t.Errorf("Host = %q, want local-tailscaled.sock", host)
	}
	if origin != "" || referer != "" {
		t.Errorf("Origin/Referer not stripped: Origin=%q Referer=%q", origin, referer)
	}

	// 5. Verify FlushInterval: -1 streams chunks immediately before EOF.
	streamCtx, streamCancel := context.WithTimeout(ctx, 3*time.Second)
	defer streamCancel()
	sreq, err := http.NewRequestWithContext(streamCtx, http.MethodGet, controlBaseURL+"/localapi/v0/stream-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	sresp, err := hc.Do(sreq)
	if err != nil {
		t.Fatalf("GET stream-test: %v", err)
	}
	line, err := bufio.NewReader(sresp.Body).ReadString('\n')
	streamCancel()
	sresp.Body.Close()
	if err != nil {
		t.Fatalf("read streamed chunk: %v", err)
	}
	if strings.TrimSpace(line) != "chunk-1" {
		t.Errorf("streamed chunk = %q, want chunk-1", line)
	}

	// 6. Verify listenControlSocket refuses to stomp a live listening socket.
	if ln2, err := listenControlSocket(sockPath); err == nil {
		ln2.Close()
		t.Fatalf("listenControlSocket(%q) unexpectedly succeeded over a live listener", sockPath)
	} else if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("live socket error = %v, want 'already in use'", err)
	}
}
