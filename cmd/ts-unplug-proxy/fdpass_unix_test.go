//go:build unix

package main

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// recvPassedConn mirrors OpenSSH's mm_receive_fd: reads one dummy byte plus
// an SCM_RIGHTS file descriptor from an AF_UNIX socketpair and wraps it as a
// net.Conn.
func recvPassedConn(t *testing.T, sockFD int) net.Conn {
	t.Helper()
	buf := make([]byte, 1)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := syscall.Recvmsg(sockFD, buf, oob, 0)
	if err != nil {
		t.Fatalf("Recvmsg: %v", err)
	}
	if n != 1 {
		t.Fatalf("Recvmsg data bytes = %d, want 1", n)
	}
	cmsgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		t.Fatalf("ParseSocketControlMessage: %v", err)
	}
	if len(cmsgs) != 1 {
		t.Fatalf("control messages = %d, want 1", len(cmsgs))
	}
	fds, err := syscall.ParseUnixRights(&cmsgs[0])
	if err != nil {
		t.Fatalf("ParseUnixRights: %v", err)
	}
	if len(fds) != 1 {
		t.Fatalf("received fds = %d, want 1", len(fds))
	}
	f := os.NewFile(uintptr(fds[0]), "passed-tcp-conn")
	defer f.Close()
	c, err := net.FileConn(f)
	if err != nil {
		t.Fatalf("net.FileConn: %v", err)
	}
	return c
}

func TestPassConnFDOverSOCKS5AndHTTP(t *testing.T) {
	const banner = "SSH-2.0-OpenSSH_fdpass_test\r\n"
	targetLn, _ := startBannerEchoServer(t, banner)

	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socksLn.Close()
	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer httpLn.Close()

	serveErr := make(chan serveResult, 2)
	dialTarget := func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", targetLn.Addr().String())
	}
	startSOCKS5Proxy(context.Background(), socksLn, dialTarget, serveErr)
	startHTTPProxy(context.Background(), httpLn, dialTarget, serveErr)

	modes := []struct {
		name string
		dial func(context.Context) (net.Conn, error)
	}{
		{
			name: "socks5",
			dial: func(ctx context.Context) (net.Conn, error) {
				return dialSOCKS5(ctx, socksLn.Addr().String(), "gxa.wild-dojo.ts.net", 22)
			},
		},
		{
			name: "http-connect",
			dial: func(ctx context.Context) (net.Conn, error) {
				return dialHTTPConnect(ctx, httpLn.Addr().String(), "gxa.wild-dojo.ts.net", 22)
			},
		},
	}

	for _, tc := range modes {
		t.Run(tc.name, func(t *testing.T) {
			pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
			if err != nil {
				t.Fatalf("Socketpair: %v", err)
			}
			defer syscall.Close(pair[0])
			defer syscall.Close(pair[1])

			conn, err := tc.dial(context.Background())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			if err := passConnFD(pair[1], conn); err != nil {
				conn.Close()
				t.Fatalf("passConnFD: %v", err)
			}
			// Simulate helper process exiting immediately after passing FD.
			conn.Close()

			passed := recvPassedConn(t, pair[0])
			defer passed.Close()

			gotBanner := make([]byte, len(banner))
			if _, err := io.ReadFull(passed, gotBanner); err != nil {
				t.Fatalf("read banner on passed fd: %v", err)
			}
			if string(gotBanner) != banner {
				t.Fatalf("banner = %q, want %q", string(gotBanner), banner)
			}
			if _, err := io.WriteString(passed, "fdpass-echo"); err != nil {
				t.Fatalf("write on passed fd: %v", err)
			}
			if err := passed.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatalf("CloseWrite on passed fd: %v", err)
			}
			gotEcho, err := io.ReadAll(passed)
			if err != nil {
				t.Fatalf("read echo on passed fd: %v", err)
			}
			if string(gotEcho) != "fdpass-echo" {
				t.Fatalf("echo = %q, want fdpass-echo", string(gotEcho))
			}
		})
	}
}

func TestPassConnFDRejectsNonSocket(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	err = passConnFD(int(w.Fd()), conn)
	if err == nil || !strings.Contains(err.Error(), "ProxyUseFdpass") {
		t.Fatalf("passConnFD on pipe = %v, want ProxyUseFdpass hint", err)
	}
}

// TestOpenSSHProxyUseFdpassEndToEnd verifies wire compatibility with the real
// system ssh binary using ProxyUseFdpass yes + `ts-unplug-proxy connect -fdpass`.
func TestOpenSSHProxyUseFdpassEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ssh build/e2e test in -short mode")
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh not installed")
	}

	binPath := filepath.Join(t.TempDir(), "ts-unplug-proxy")
	build := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// Mock SSH endpoint: sends a recognizable SSH banner, reads the client's
	// SSH banner, then closes cleanly. If OpenSSH logs our banner, we have
	// proven end-to-end ProxyCommand + ProxyUseFdpass + SOCKS5h + zero over-read.
	const mockBanner = "SSH-2.0-ts_unplug_proxy_fdpass_verified\r\n"
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetLn.Close()

	clientBanner := make(chan string, 1)
	go func() {
		c, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.WriteString(c, mockBanner)
		buf := make([]byte, 128)
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, _ := c.Read(buf)
		clientBanner <- string(buf[:n])
	}()

	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socksLn.Close()

	dialedHost := make(chan string, 1)
	serveErr := make(chan serveResult, 1)
	startSOCKS5Proxy(context.Background(), socksLn, func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialedHost <- addr
		var d net.Dialer
		return d.DialContext(ctx, "tcp", targetLn.Addr().String())
	}, serveErr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshPath,
		"-F", "/dev/null",
		"-v",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ProxyUseFdpass=yes",
		"-o", "ProxyCommand="+binPath+" connect -fdpass -socks5 "+socksLn.Addr().String()+" %h %p",
		"fakehost.wild-dojo.ts.net",
	)
	out, _ := cmd.CombinedOutput()
	output := string(out)

	select {
	case gotAddr := <-dialedHost:
		if gotAddr != "fakehost.wild-dojo.ts.net:22" {
			t.Fatalf("proxy dialed %q, want fakehost.wild-dojo.ts.net:22", gotAddr)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("SOCKS5 proxy was never dialed by ssh; ssh output:\n%s", output)
	}

	if !strings.Contains(output, "ts_unplug_proxy_fdpass_verified") {
		t.Fatalf("ssh did not receive remote SSH banner over passed FD; ssh output:\n%s", output)
	}

	select {
	case cb := <-clientBanner:
		if !strings.HasPrefix(cb, "SSH-2.0-") {
			t.Fatalf("mock target received client banner %q, want SSH-2.0-...", cb)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("mock target never received ssh client banner; ssh output:\n%s", output)
	}
}
