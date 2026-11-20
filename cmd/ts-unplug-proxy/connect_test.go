package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseTargetArgs(t *testing.T) {
	tests := []struct {
		args     []string
		wantHost string
		wantPort uint16
		wantErr  bool
	}{
		{[]string{"gxa.wild-dojo.ts.net", "22"}, "gxa.wild-dojo.ts.net", 22, false},
		{[]string{"gxa.wild-dojo.ts.net:22"}, "gxa.wild-dojo.ts.net", 22, false},
		{[]string{"100.64.0.1", "2222"}, "100.64.0.1", 2222, false},
		{[]string{"100.64.0.1:2222"}, "100.64.0.1", 2222, false},
		{[]string{"fd7a:115c:a1e0::1", "22"}, "fd7a:115c:a1e0::1", 22, false},
		{[]string{"[fd7a:115c:a1e0::1]", "22"}, "fd7a:115c:a1e0::1", 22, false},
		{[]string{"[fd7a:115c:a1e0::1]:22"}, "fd7a:115c:a1e0::1", 22, false},
		{nil, "", 0, true},
		{[]string{"onlyhost"}, "", 0, true},
		{[]string{"host", "0"}, "", 0, true},
		{[]string{"host", "65536"}, "", 0, true},
		{[]string{"host", "ssh"}, "", 0, true},
		{[]string{"bad host", "22"}, "", 0, true},
		{[]string{"host", "22", "extra"}, "", 0, true},
		{[]string{"", "22"}, "", 0, true},
	}
	for _, tc := range tests {
		host, port, err := parseTargetArgs(tc.args)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseTargetArgs(%v) = (%q, %d), want error", tc.args, host, port)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTargetArgs(%v) unexpected error: %v", tc.args, err)
			continue
		}
		if host != tc.wantHost || port != tc.wantPort {
			t.Errorf("parseTargetArgs(%v) = (%q, %d), want (%q, %d)", tc.args, host, port, tc.wantHost, tc.wantPort)
		}
	}
}

func startBannerEchoServer(t *testing.T, banner string) (net.Listener, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	errc := make(chan error, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				// Send SSH-style banner immediately on accept to verify zero
				// over-read during SOCKS5 / HTTP CONNECT handshake.
				if _, err := io.WriteString(conn, banner); err != nil {
					errc <- err
					return
				}
				_, err := io.Copy(conn, conn)
				errc <- err
			}(c)
		}
	}()
	return ln, errc
}

func TestDialSOCKS5HostIPv4IPv6AndBanner(t *testing.T) {
	const banner = "SSH-2.0-OpenSSH_10.5_test\r\n"
	targetLn, _ := startBannerEchoServer(t, banner)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()

	seenAddrs := make(chan string, 4)
	serveErr := make(chan serveResult, 1)
	startSOCKS5Proxy(context.Background(), proxyLn, func(ctx context.Context, network, addr string) (net.Conn, error) {
		seenAddrs <- addr
		var d net.Dialer
		return d.DialContext(ctx, "tcp", targetLn.Addr().String())
	}, serveErr)

	cases := []struct {
		host     string
		port     uint16
		wantAddr string
	}{
		{"gxa.wild-dojo.ts.net", 22, "gxa.wild-dojo.ts.net:22"},
		{"100.64.1.2", 2222, "100.64.1.2:2222"},
		{"fd7a:115c:a1e0::1", 22, "[fd7a:115c:a1e0::1]:22"},
	}

	for _, tc := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := dialSOCKS5(ctx, proxyLn.Addr().String(), tc.host, tc.port)
		cancel()
		if err != nil {
			t.Fatalf("dialSOCKS5(%s:%d): %v", tc.host, tc.port, err)
		}

		// Verify remote received the exact hostname/IP (SOCKS5h, never resolved locally).
		if gotAddr := <-seenAddrs; gotAddr != tc.wantAddr {
			t.Errorf("proxy dialed %q, want %q", gotAddr, tc.wantAddr)
		}

		// Verify immediate banner was NOT over-read during handshake.
		gotBanner := make([]byte, len(banner))
		if _, err := io.ReadFull(conn, gotBanner); err != nil {
			t.Fatalf("read banner for %s: %v", tc.host, err)
		}
		if string(gotBanner) != banner {
			t.Fatalf("banner = %q, want %q", string(gotBanner), banner)
		}

		// Verify bidirectional echo works even after ctx is canceled (deadline cleared).
		if _, err := io.WriteString(conn, "ping-pong"); err != nil {
			t.Fatalf("write echo: %v", err)
		}
		if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
		echoed, err := io.ReadAll(conn)
		conn.Close()
		if err != nil {
			t.Fatalf("read echo: %v", err)
		}
		if string(echoed) != "ping-pong" {
			t.Fatalf("echo = %q, want ping-pong", string(echoed))
		}
	}
}

func TestDialSOCKS5Errors(t *testing.T) {
	// 1. Proxy fails target dial -> RFC 1928 general failure error message.
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()

	serveErr := make(chan serveResult, 1)
	startSOCKS5Proxy(context.Background(), proxyLn, func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("no such host")
	}, serveErr)

	_, err = dialSOCKS5(context.Background(), proxyLn.Addr().String(), "missing.ts.net", 22)
	if err == nil || !strings.Contains(err.Error(), "general SOCKS server failure") {
		t.Fatalf("dialSOCKS5 dial-fail err = %v, want general SOCKS server failure", err)
	}

	// 2. Closed proxy port -> actionable "is ts-unplug-proxy running?" message.
	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closedLn.Addr().String()
	closedLn.Close()

	_, err = dialSOCKS5(context.Background(), closedAddr, "gxa.ts.net", 22)
	if err == nil || !strings.Contains(err.Error(), "is ts-unplug-proxy running?") {
		t.Fatalf("dialSOCKS5 refused err = %v, want running hint", err)
	}
}

func TestDialHTTPConnectBannerAndErrors(t *testing.T) {
	const banner = "SSH-2.0-OpenSSH_HTTP_test\r\n"
	targetLn, _ := startBannerEchoServer(t, banner)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()

	serveErr := make(chan serveResult, 1)
	startHTTPProxy(context.Background(), proxyLn, func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "fail.") {
			return nil, errors.New("tailnet DNS lookup for \"fail.ts.net\": rcode RCodeNameError")
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", targetLn.Addr().String())
	}, serveErr)

	// Success + immediate banner preservation + cleared deadline after cancel.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := dialHTTPConnect(ctx, proxyLn.Addr().String(), "gxa.wild-dojo.ts.net", 22)
	cancel()
	if err != nil {
		t.Fatalf("dialHTTPConnect: %v", err)
	}
	defer conn.Close()

	gotBanner := make([]byte, len(banner))
	if _, err := io.ReadFull(conn, gotBanner); err != nil {
		t.Fatalf("read banner: %v", err)
	}
	if string(gotBanner) != banner {
		t.Fatalf("banner = %q, want %q", string(gotBanner), banner)
	}
	if _, err := io.WriteString(conn, "http-tunnel-ok"); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	echoed, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(echoed) != "http-tunnel-ok" {
		t.Fatalf("echo = %q, want http-tunnel-ok", string(echoed))
	}

	// Failure surfaces Tailscale-Connect-Error header directly.
	_, err = dialHTTPConnect(context.Background(), proxyLn.Addr().String(), "fail.ts.net", 22)
	if err == nil || !strings.Contains(err.Error(), "RCodeNameError") {
		t.Fatalf("dialHTTPConnect fail err = %v, want RCodeNameError detail", err)
	}
}

func TestCheckStatusFastFail(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantErrSub string
	}{
		{
			name:       "ready 200",
			statusCode: http.StatusOK,
			body:       `{"ready":true,"serving":true,"backend_state":"Running"}`,
		},
		{
			name:       "starting 503 proceeds",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"ready":false,"serving":false,"backend_state":"Starting"}`,
		},
		{
			name:       "NeedsLogin with auth_url fails fast",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"ready":false,"serving":false,"backend_state":"NeedsLogin","auth_url":"https://login.tailscale.com/a/abc123"}`,
			wantErrSub: "https://login.tailscale.com/a/abc123",
		},
		{
			name:       "NeedsLogin without auth_url fails fast",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"ready":false,"serving":false,"backend_state":"NeedsLogin","auth_url":""}`,
			wantErrSub: "needs Tailscale login",
		},
		{
			name:       "Stopped fails fast",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"ready":false,"serving":false,"backend_state":"Stopped"}`,
			wantErrSub: "stopped",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/status" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(tc.statusCode)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			addr := strings.TrimPrefix(srv.URL, "http://")
			err := checkStatusFastFail(context.Background(), addr)
			if tc.wantErrSub == "" {
				if err != nil {
					t.Errorf("checkStatusFastFail() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("checkStatusFastFail() = %v, want substring %q", err, tc.wantErrSub)
			}
		})
	}
}

func TestRelayStdioHalfCloseAndRemoteClose(t *testing.T) {
	// 1. Stdin hits EOF first; remote reads until EOF, then replies -> no truncation.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		req, _ := io.ReadAll(c)
		_, _ = io.WriteString(c, "got:"+string(req))
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := relayStdio(conn, strings.NewReader("stdin-payload"), &out); err != nil {
		t.Fatalf("relayStdio half-close err: %v", err)
	}
	if out.String() != "got:stdin-payload" {
		t.Fatalf("stdout = %q, want got:stdin-payload", out.String())
	}

	// 2. Remote closes first while stdin stays open -> relayStdio must return promptly.
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()

	go func() {
		c, err := ln2.Accept()
		if err != nil {
			return
		}
		_, _ = io.WriteString(c, "bye")
		c.Close()
	}()

	conn2, err := net.Dial("tcp", ln2.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe() // never closed until after relayStdio returns
	defer pw.Close()
	defer pr.Close()

	done := make(chan error, 1)
	var out2 bytes.Buffer
	go func() {
		done <- relayStdio(conn2, pr, &out2)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relayStdio remote-close err: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relayStdio hung waiting on open stdin after remote closed")
	}
	if out2.String() != "bye" {
		t.Fatalf("stdout = %q, want bye", out2.String())
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestRelayStdioStdinErrorPropagates(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(io.Discard, c)
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("boom-stdin")
	var out bytes.Buffer
	got := relayStdio(conn, errReader{err: sentinel}, &out)
	if !errors.Is(got, sentinel) {
		t.Fatalf("relayStdio stdin error = %v, want %v", got, sentinel)
	}
}

func TestCheckStatusFastFailMalformedAndMissingState(t *testing.T) {
	for _, body := range []string{"{not-json", `{"ready":false}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, body)
		}))
		addr := strings.TrimPrefix(srv.URL, "http://")
		err := checkStatusFastFail(context.Background(), addr)
		srv.Close()
		if err != nil {
			t.Errorf("checkStatusFastFail(%q) = %v, want nil", body, err)
		}
	}
}

func TestRunConnectExitCodesAndStdoutPurity(t *testing.T) {
	// Closed loopback port so proxy-down test fails deterministically.
	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closedLn.Addr().String()
	closedLn.Close()

	// NeedsLogin status server for fast-fail CLI test.
	statusSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"ready":false,"backend_state":"NeedsLogin","auth_url":"https://login.tailscale.com/a/test"}`)
	}))
	defer statusSrv.Close()
	statusAddr := strings.TrimPrefix(statusSrv.URL, "http://")

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantErrSub string
	}{
		{"help -h", []string{"-h"}, 0, "USAGE"},
		{"help --help", []string{"--help"}, 0, "USAGE"},
		{"unknown flag", []string{"-bogus"}, 2, "flag provided but not defined"},
		{"both socks5 and http", []string{"-socks5", "127.0.0.1:1080", "-http", "127.0.0.1:8080", "h", "22"}, 2, "only one of -socks5 or -http"},
		{"zero timeout", []string{"-timeout", "0s", "h", "22"}, 2, "-timeout must be positive"},
		{"missing target", nil, 2, "expected <host> <port>"},
		{"bad port", []string{"h", "99999"}, 2, "invalid target port"},
		{"proxy down", []string{"-socks5", closedAddr, "gxa.wild-dojo.ts.net", "22"}, 1, "is ts-unplug-proxy running?"},
		{"status NeedsLogin fast-fail", []string{"-socks5", closedAddr, "-status", statusAddr, "gxa.wild-dojo.ts.net", "22"}, 1, "https://login.tailscale.com/a/test"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runConnectIO(tc.args, strings.NewReader(""), &stdout, &stderr, -1)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d (stderr=%q)", code, tc.wantCode, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout was not empty: %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.wantErrSub) {
				t.Errorf("stderr = %q, want substring %q", stderr.String(), tc.wantErrSub)
			}
		})
	}
}
