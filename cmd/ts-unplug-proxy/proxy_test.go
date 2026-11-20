package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAdvertiseTags(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"", nil, false},
		{" , ", nil, false},
		{"tag:proxy", []string{"tag:proxy"}, false},
		{"tag:proxy, tag:laptop-dev", []string{"tag:proxy", "tag:laptop-dev"}, false},
		{"proxy", nil, true},
		{"tag:", nil, true},
		{"tag:foo bar", nil, true},
		{"tag:ok,bad", nil, true},
	}
	for _, tc := range tests {
		got, err := parseAdvertiseTags(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseAdvertiseTags(%q) succeeded (%v), want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAdvertiseTags(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parseAdvertiseTags(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseAdvertiseTags(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestTailnetDialerTimeout(t *testing.T) {
	if got := (&tailnetDialer{}).timeout(); got != defaultDialTimeout {
		t.Errorf("zero dialTimeout = %v, want %v", got, defaultDialTimeout)
	}
	if got := (&tailnetDialer{dialTimeout: 45 * time.Second}).timeout(); got != 45*time.Second {
		t.Errorf("custom dialTimeout = %v, want 45s", got)
	}
}

func TestStartSOCKS5ProxyUsesLifecycleContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Verify that startSOCKS5Proxy replaces tailscale's hardcoded 5s dial
	// context with our lifecycle context (no 5s deadline, and cancels when
	// serverCtx is canceled).
	serverCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()

	dialStarted := make(chan context.Context, 1)
	dialCanceled := make(chan error, 1)
	serveErr := make(chan serveResult, 1)

	startSOCKS5Proxy(serverCtx, ln, func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialStarted <- ctx
		<-ctx.Done()
		dialCanceled <- ctx.Err()
		return nil, ctx.Err()
	}, serveErr)

	clientDone := make(chan error, 1)
	go func() {
		_, err := dialSOCKS5(context.Background(), ln.Addr().String(), "example.ts.net", 22)
		clientDone <- err
	}()

	var gotCtx context.Context
	select {
	case gotCtx = <-dialStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("dialer was never invoked")
	}

	if _, hasDeadline := gotCtx.Deadline(); hasDeadline {
		t.Fatal("SOCKS5 dialer context unexpectedly has upstream 5s deadline; expected lifecycle ctx without forced 5s cap")
	}

	cancelServer()
	select {
	case err := <-dialCanceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dialer cancel err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight SOCKS5 dial was not canceled on server shutdown")
	}
	<-clientDone
}

func TestHTTPConnectHalfCloseNoTruncation(t *testing.T) {
	// Target server reads until client half-closes (EOF), THEN sends a large
	// response and closes. Without half-close support in httpProxyHandler,
	// the proxy tears down both sockets as soon as client->backend hits EOF,
	// truncating or losing the response completely.
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetLn.Close()

	wantReply := bytes.Repeat([]byte("reply-chunk-0123456789abcdef\n"), 4096) // ~118 KiB
	targetErr := make(chan error, 1)
	go func() {
		c, err := targetLn.Accept()
		if err != nil {
			targetErr <- err
			return
		}
		defer c.Close()
		gotReq, err := io.ReadAll(c)
		if err != nil {
			targetErr <- err
			return
		}
		if string(gotReq) != "hello-from-client" {
			targetErr <- errors.New("unexpected request payload: " + string(gotReq))
			return
		}
		// Pause briefly to ensure the client->backend EOF goroutine has
		// definitely finished and returned before we send the reply.
		time.Sleep(30 * time.Millisecond)
		if _, err := c.Write(wantReply); err != nil {
			targetErr <- err
			return
		}
		targetErr <- nil
	}()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()

	serveErr := make(chan serveResult, 1)
	startHTTPProxy(context.Background(), proxyLn, func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, targetLn.Addr().String())
	}, serveErr)

	conn, err := dialHTTPConnect(context.Background(), proxyLn.Addr().String(), "target.ts.net", 22)
	if err != nil {
		t.Fatalf("dialHTTPConnect: %v", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "hello-from-client"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	gotReply, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if err := <-targetErr; err != nil {
		t.Fatalf("target server error: %v", err)
	}
	if !bytes.Equal(gotReply, wantReply) {
		t.Fatalf("reply truncated/corrupted: got %d bytes, want %d bytes", len(gotReply), len(wantReply))
	}
}

func TestHTTPConnectErrorHeader(t *testing.T) {
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()

	serveErr := make(chan serveResult, 1)
	startHTTPProxy(context.Background(), proxyLn, func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("tailnet DNS lookup for \"missing.ts.net\": rcode RCodeNameError")
	}, serveErr)

	conn, err := net.Dial("tcp", proxyLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_, _ = io.WriteString(conn, "CONNECT missing.ts.net:22 HTTP/1.1\r\nHost: missing.ts.net:22\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if got := resp.Header.Get("Tailscale-Connect-Error"); !strings.Contains(got, "RCodeNameError") {
		t.Fatalf("Tailscale-Connect-Error = %q, want RCodeNameError", got)
	}
}

func TestHTTPConnectEarlyBufferedDataAndHalfClose(t *testing.T) {
	// Exercise the clientBuf.Reader.Buffered() > 0 path in httpProxyHandler:
	// the client pipelines tunneled payload bytes immediately after CONNECT
	// headers in a single write, then half-closes and expects the full reply.
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetLn.Close()

	targetGot := make(chan string, 1)
	go func() {
		c, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		bs, _ := io.ReadAll(c)
		targetGot <- string(bs)
		_, _ = io.WriteString(c, "ack:"+string(bs))
	}()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()

	serveErr := make(chan serveResult, 1)
	startHTTPProxy(context.Background(), proxyLn, func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, targetLn.Addr().String())
	}, serveErr)

	conn, err := net.Dial("tcp", proxyLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Send CONNECT request + early tunneled bytes in a single write so
	// net/http's reader buffers EARLY-DATA into clientBuf before Hijack.
	// Wait for 200 OK before calling CloseWrite (an EOF before 200 OK tells
	// net/http the client disconnected and cancels r.Context()).
	if _, err := io.WriteString(conn, "CONNECT target.ts.net:22 HTTP/1.1\r\nHost: target.ts.net:22\r\n\r\nEARLY-DATA"); err != nil {
		t.Fatal(err)
	}

	rawHeaders, err := readHTTPHeadersExact(conn, maxHTTPConnectHeaderBytes)
	if err != nil {
		t.Fatalf("read headers: %v", err)
	}
	if !strings.HasPrefix(string(rawHeaders), "HTTP/1.1 200") {
		t.Fatalf("unexpected CONNECT response: %q", string(rawHeaders))
	}

	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	reply, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if got := <-targetGot; got != "EARLY-DATA" {
		t.Fatalf("target received %q, want EARLY-DATA", got)
	}
	if string(reply) != "ack:EARLY-DATA" {
		t.Fatalf("client received %q, want ack:EARLY-DATA", string(reply))
	}
}

func TestDoctorProbeIdentityPreservesPersisted(t *testing.T) {
	dir := t.TempDir()
	prefsJSON := `{"Hostname":"laptop-proxy","AdvertiseTags":["tag:dev-proxy"]}`
	state := map[string][]byte{
		"_current-profile": []byte("profile-1234"),
		"profile-1234":     []byte(prefsJSON),
	}
	bs, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bs, 0o600); err != nil {
		t.Fatal(err)
	}

	// Unset flags inherit persisted Hostname and AdvertiseTags from -dir.
	d := &doctor{dir: dir, hostname: tsnetHostname}
	host, tags := d.probeIdentity()
	if host != "laptop-proxy" {
		t.Errorf("probeIdentity hostname = %q, want laptop-proxy", host)
	}
	if len(tags) != 1 || tags[0] != "tag:dev-proxy" {
		t.Errorf("probeIdentity tags = %v, want [tag:dev-proxy]", tags)
	}

	// Explicit CLI flags override persisted values.
	d2 := &doctor{
		dir:           dir,
		hostname:      "override-host",
		hostnameSet:   true,
		advertiseTags: []string{"tag:override"},
		tagsSet:       true,
	}
	host2, tags2 := d2.probeIdentity()
	if host2 != "override-host" {
		t.Errorf("override hostname = %q, want override-host", host2)
	}
	if len(tags2) != 1 || tags2[0] != "tag:override" {
		t.Errorf("override tags = %v, want [tag:override]", tags2)
	}
}
