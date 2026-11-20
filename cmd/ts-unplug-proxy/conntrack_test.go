package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"tailscale.com/types/nettype"
)

func TestConnTrackerLifecycleAndBytes(t *testing.T) {
	tr := newConnTracker()
	if tr.Count() != 0 {
		t.Fatalf("initial Count() = %d, want 0", tr.Count())
	}
	if snap := tr.Snapshot(); snap == nil || len(snap) != 0 {
		t.Fatalf("initial Snapshot() = %#v, want empty non-nil slice", snap)
	}

	c1, s1 := net.Pipe()
	defer s1.Close()
	c2, s2 := net.Pipe()
	defer s2.Close()

	tc1 := tr.Wrap("socks5", "alpha.ts.net:22", c1)
	tc2 := tr.Wrap("http", "beta.ts.net:443", c2)

	if tr.Count() != 2 {
		t.Fatalf("Count() = %d, want 2", tr.Count())
	}

	// Send bytes out (Write on tc1) and receive bytes in (Read on tc1).
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 5)
		if _, err := io.ReadFull(s1, buf); err != nil {
			done <- err
			return
		}
		if string(buf) != "hello" {
			done <- errors.New("unexpected payload: " + string(buf))
			return
		}
		_, err := s1.Write([]byte("world!!!"))
		done <- err
	}()

	if _, err := tc1.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := make([]byte, 8)
	if _, err := io.ReadFull(tc1, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("peer goroutine: %v", err)
	}

	snap := tr.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(snap))
	}
	if snap[0].ID >= snap[1].ID {
		t.Errorf("Snapshot not sorted by ID: %d >= %d", snap[0].ID, snap[1].ID)
	}
	if snap[0].Proto != "socks5" || snap[0].Target != "alpha.ts.net:22" {
		t.Errorf("snap[0] = %+v", snap[0])
	}
	if snap[0].BytesOut != 5 || snap[0].BytesIn != 8 {
		t.Errorf("snap[0] bytes out/in = %d/%d, want 5/8", snap[0].BytesOut, snap[0].BytesIn)
	}
	if snap[1].Proto != "http" || snap[1].Target != "beta.ts.net:443" {
		t.Errorf("snap[1] = %+v", snap[1])
	}

	// Closing multiple times must be idempotent.
	_ = tc1.Close()
	_ = tc1.Close()
	if tr.Count() != 1 {
		t.Fatalf("Count after closing tc1 = %d, want 1", tr.Count())
	}

	_ = tc2.Close()
	if tr.Count() != 0 {
		t.Fatalf("Count after closing tc2 = %d, want 0", tr.Count())
	}
}

func TestTrackedConnImplementsHalfCloser(t *testing.T) {
	tr := newConnTracker()
	c, s := net.Pipe()
	defer s.Close()
	tc := tr.Wrap("forward", "example.ts.net:80", c)
	defer tc.Close()

	if _, ok := tc.(halfCloser); !ok {
		t.Fatal("trackedConn does not implement halfCloser")
	}
	if _, ok := tc.(nettype.HalfCloser); !ok {
		t.Fatal("trackedConn does not implement nettype.HalfCloser")
	}
}

func TestTrackedConnSOCKS5HalfClose(t *testing.T) {
	// Verify TCP half-close works end-to-end through socks5.Server when the
	// backend connection is wrapped in trackedConn.
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetLn.Close()

	wantReply := bytes.Repeat([]byte("socks5-half-close-reply-"), 512)
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
		if string(req) != "client-request" {
			targetErr <- errors.New("unexpected request: " + string(req))
			return
		}
		_, err = c.Write(wantReply)
		targetErr <- err
	}()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()

	tr := newConnTracker()
	serveErr := make(chan serveResult, 1)
	startSOCKS5Proxy(context.Background(), proxyLn, tr.WrapDial("socks5", func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, targetLn.Addr().String())
	}), serveErr)

	conn, err := dialSOCKS5(context.Background(), proxyLn.Addr().String(), "target.ts.net", 22)
	if err != nil {
		t.Fatalf("dialSOCKS5: %v", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "client-request"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	gotReply, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if err := <-targetErr; err != nil {
		t.Fatalf("target error: %v", err)
	}
	if !bytes.Equal(gotReply, wantReply) {
		t.Fatalf("reply mismatch: got %d bytes, want %d bytes", len(gotReply), len(wantReply))
	}

	// Wait briefly for the SOCKS5 handler goroutine to close backend conn.
	deadline := time.Now().Add(2 * time.Second)
	for tr.Count() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if tr.Count() != 0 {
		t.Fatalf("tracked connections leaked: Count() = %d, want 0", tr.Count())
	}
}

func TestConnTrackerWrapDialDoesNotHoldLockDuringDial(t *testing.T) {
	tr := newConnTracker()
	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{})

	wrapped := tr.WrapDial("http", func(ctx context.Context, network, addr string) (net.Conn, error) {
		close(dialStarted)
		<-releaseDial
		c, s := net.Pipe()
		_ = s.Close()
		return c, nil
	})

	done := make(chan net.Conn, 1)
	go func() {
		conn, _ := wrapped(context.Background(), "tcp", "slow.ts.net:443")
		done <- conn
	}()

	<-dialStarted
	// Must not deadlock while dial is blocked.
	if got := tr.Count(); got != 0 {
		t.Fatalf("Count during in-flight dial = %d, want 0", got)
	}
	_ = tr.Snapshot()

	close(releaseDial)
	conn := <-done
	if conn == nil {
		t.Fatal("expected non-nil conn")
	}
	if got := tr.Count(); got != 1 {
		t.Fatalf("Count after dial = %d, want 1", got)
	}
	_ = conn.Close()
	if got := tr.Count(); got != 0 {
		t.Fatalf("Count after close = %d, want 0", got)
	}
}

func TestConnTrackerConcurrentRace(t *testing.T) {
	tr := newConnTracker()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, s := net.Pipe()
			tc := tr.Wrap("forward", "peer.ts.net:7687", c)
			go func() {
				_, _ = io.Copy(io.Discard, s)
				_ = s.Close()
			}()
			_, _ = tc.Write([]byte("payload"))
			_ = tr.Snapshot()
			_ = tr.Count()
			_ = tc.Close()
		}()
	}
	wg.Wait()
	if tr.Count() != 0 {
		t.Fatalf("final Count() = %d, want 0", tr.Count())
	}
}
