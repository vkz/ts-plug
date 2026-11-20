package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestParseForwardSpec(t *testing.T) {
	valid := []struct {
		raw        string
		wantLocal  string
		wantRemote string
	}{
		{
			"127.0.0.1:7687=neo4j.wild-dojo.ts.net:7687",
			"127.0.0.1:7687",
			"neo4j.wild-dojo.ts.net:7687",
		},
		{
			"[::1]:5432=[fd7a:115c:a1e0::1]:5432",
			"[::1]:5432",
			"[fd7a:115c:a1e0::1]:5432",
		},
		{
			"localhost:8080=100.64.1.2:80",
			"localhost:8080",
			"100.64.1.2:80",
		},
		{
			" 127.0.0.1:1 = host.ts.net:65535 ",
			"127.0.0.1:1",
			"host.ts.net:65535",
		},
	}
	for _, tc := range valid {
		got, err := parseForwardSpec(tc.raw)
		if err != nil {
			t.Errorf("parseForwardSpec(%q) unexpected error: %v", tc.raw, err)
			continue
		}
		if got.local != tc.wantLocal || got.remote != tc.wantRemote {
			t.Errorf("parseForwardSpec(%q) = %+v, want %q=%q", tc.raw, got, tc.wantLocal, tc.wantRemote)
		}
	}

	invalid := []string{
		"",
		"127.0.0.1:7687",
		"=host.ts.net:7687",
		"127.0.0.1:7687=",
		":7687=host.ts.net:7687",
		"127.0.0.1:7687=:7687",
		"127.0.0.1:0=host.ts.net:7687",
		"127.0.0.1:7687=host.ts.net:0",
		"127.0.0.1:65536=host.ts.net:7687",
		"127.0.0.1:7687=host.ts.net:65536",
		"127.0.0.1:abc=host.ts.net:7687",
		"127.0.0.1:7687=host.ts.net:abc",
		"127.0.0.1:7687=host.ts.net:7687=extra",
		"bad host:7687=host.ts.net:7687",
		"127.0.0.1:7687=bad host:7687",
		"127.0.0.1=host.ts.net:7687",
		"127.0.0.1:7687=host.ts.net",
	}
	for _, raw := range invalid {
		if got, err := parseForwardSpec(raw); err == nil {
			t.Errorf("parseForwardSpec(%q) succeeded (%+v), want error", raw, got)
		}
	}
}

func TestForwardListRejectsDuplicateLocal(t *testing.T) {
	var fl forwardList
	if err := fl.Set("127.0.0.1:7687=neo4j.ts.net:7687"); err != nil {
		t.Fatalf("first Set failed: %v", err)
	}
	if err := fl.Set("127.0.0.1:7687=other.ts.net:7687"); err == nil {
		t.Error("duplicate IPv4 local bind succeeded, want error")
	}
	if err := fl.Set("[::1]:5432=pg.ts.net:5432"); err != nil {
		t.Fatalf("IPv6 Set failed: %v", err)
	}
	if err := fl.Set("[0:0:0:0:0:0:0:1]:5432=pg2.ts.net:5432"); err == nil {
		t.Error("canonical duplicate IPv6 local bind succeeded, want error")
	}
	if len(fl) != 2 {
		t.Fatalf("len(fl) = %d, want 2", len(fl))
	}
}

func TestStartForwardHalfCloseAndTracking(t *testing.T) {
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetLn.Close()

	wantReply := bytes.Repeat([]byte("forward-reply-payload-"), 1024)
	targetReadReq := make(chan struct{})
	targetRelease := make(chan struct{})
	targetErr := make(chan error, 1)

	go func() {
		c, err := targetLn.Accept()
		if err != nil {
			targetErr <- err
			return
		}
		defer c.Close()
		req, err := io.ReadAll(c)
		if err != nil {
			targetErr <- err
			return
		}
		if string(req) != "hello-forward" {
			targetErr <- errors.New("unexpected payload: " + string(req))
			return
		}
		close(targetReadReq)
		<-targetRelease
		_, err = c.Write(wantReply)
		targetErr <- err
	}()

	fwdLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer fwdLn.Close()

	tr := newConnTracker()
	serveErr := make(chan serveResult, 1)
	spec := forwardSpec{local: fwdLn.Addr().String(), remote: "neo4j.wild-dojo.ts.net:7687"}

	dialedCh := make(chan string, 1)
	startForward(context.Background(), fwdLn, spec, tr.WrapDial("forward", func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialedCh <- addr
		var d net.Dialer
		return d.DialContext(ctx, network, targetLn.Addr().String())
	}), false, serveErr)

	conn, err := net.Dial("tcp", fwdLn.Addr().String())
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "hello-forward"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	// Waiting for target to read the full request + half-close EOF guarantees
	// the tracked connection is registered and bytes_out is recorded while the
	// connection is still open waiting for targetRelease.
	<-targetReadReq

	snap := tr.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("active connections = %d, want 1", len(snap))
	}
	if snap[0].Proto != "forward" || snap[0].Target != spec.remote {
		t.Errorf("tracked snapshot = %+v, want proto=forward target=%q", snap[0], spec.remote)
	}
	if snap[0].BytesOut != int64(len("hello-forward")) {
		t.Errorf("bytes_out = %d, want %d", snap[0].BytesOut, len("hello-forward"))
	}

	close(targetRelease)

	gotReply, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if err := <-targetErr; err != nil {
		t.Fatalf("target error: %v", err)
	}
	if dialedAddr := <-dialedCh; dialedAddr != spec.remote {
		t.Errorf("dialedAddr = %q, want %q", dialedAddr, spec.remote)
	}
	if !bytes.Equal(gotReply, wantReply) {
		t.Fatalf("reply truncated/corrupted: got %d bytes, want %d bytes", len(gotReply), len(wantReply))
	}

	deadline := time.Now().Add(2 * time.Second)
	for tr.Count() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if tr.Count() != 0 {
		t.Fatalf("tracked connections leaked: Count() = %d, want 0", tr.Count())
	}
}
