package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/textproto"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultConnectSOCKS5      = "127.0.0.1:1080"
	defaultConnectTimeout     = defaultDialTimeout + 5*time.Second
	maxHTTPConnectHeaderBytes = 16 * 1024
	statusCheckTimeout        = 1 * time.Second
)

const connectUsageText = `ts-unplug-proxy connect - tunnel stdio (or pass a socket FD) through a local
ts-unplug-proxy listener, designed for OpenSSH ProxyCommand / ProxyUseFdpass.

USAGE
  ts-unplug-proxy connect [options] <host> <port>
  ts-unplug-proxy connect [options] <host>:<port>

  Note: 'connect' must be the first argument after ts-unplug-proxy.

OPTIONS
  -socks5 <addr>    SOCKS5 proxy address (default 127.0.0.1:1080 when -http is unset)
  -http <addr>      HTTP CONNECT proxy address (mutually exclusive with -socks5)
  -status <addr>    optional PAC/status address (e.g. 127.0.0.1:8099) to fail fast
                    with the Tailscale login URL when the proxy is in NeedsLogin
  -fdpass           pass the connected TCP socket FD to stdout via SCM_RIGHTS for
                    OpenSSH "ProxyUseFdpass yes" and exit immediately (Unix only)
  -timeout <dur>    timeout for connecting and completing the proxy handshake
                    (default 35s, slightly above the proxy's 30s -dial-timeout;
                    raise this above -dial-timeout if you increase it)
  -v                log debug details to stderr

EXAMPLES
  # OpenSSH (~/.ssh/config), zero lingering helper process:
  Match final host *.wild-dojo.ts.net,*.ts.net
    ProxyCommand ts-unplug-proxy connect -fdpass -status 127.0.0.1:8099 %h %p
    ProxyUseFdpass yes
    ServerAliveInterval 30

  # Standard stdio relay (no ProxyUseFdpass):
  ProxyCommand ts-unplug-proxy connect %h %p
`

func runConnect(args []string) int {
	return runConnectIO(args, os.Stdin, os.Stdout, os.Stderr, int(os.Stdout.Fd()))
}

func runConnectIO(args []string, stdin io.Reader, stdout, stderr io.Writer, fdpassFD int) int {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = io.WriteString(stderr, connectUsageText)
	}

	socksAddr := fs.String("socks5", "", "SOCKS5 proxy address (default 127.0.0.1:1080 when -http is unset)")
	httpAddr := fs.String("http", "", "HTTP CONNECT proxy address")
	statusAddr := fs.String("status", "", "optional /status listen address for fast NeedsLogin errors")
	fdpass := fs.Bool("fdpass", false, "pass connected socket fd to stdout (OpenSSH ProxyUseFdpass yes)")
	timeout := fs.Duration("timeout", defaultConnectTimeout, "proxy connect and handshake timeout")
	verbose := fs.Bool("v", false, "log debug details to stderr")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *verbose {
		slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}
	if *socksAddr != "" && *httpAddr != "" {
		fmt.Fprintln(stderr, "ts-unplug-proxy connect: specify only one of -socks5 or -http")
		return 2
	}
	if *socksAddr == "" && *httpAddr == "" {
		*socksAddr = defaultConnectSOCKS5
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "ts-unplug-proxy connect: -timeout must be positive")
		return 2
	}

	host, port, err := parseTargetArgs(fs.Args())
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy connect: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *statusAddr != "" {
		if err := checkStatusFastFail(ctx, *statusAddr); err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy connect: %v\n", err)
			return 1
		}
	}

	dialCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	var conn net.Conn
	if *httpAddr != "" {
		slog.Debug("dialing via HTTP CONNECT",
			slog.String("proxy", *httpAddr),
			slog.String("host", host),
			slog.Uint64("port", uint64(port)),
		)
		conn, err = dialHTTPConnect(dialCtx, *httpAddr, host, port)
	} else {
		slog.Debug("dialing via SOCKS5",
			slog.String("proxy", *socksAddr),
			slog.String("host", host),
			slog.Uint64("port", uint64(port)),
		)
		conn, err = dialSOCKS5(dialCtx, *socksAddr, host, port)
	}
	if err != nil {
		switch {
		case ctx.Err() != nil:
			fmt.Fprintf(stderr, "ts-unplug-proxy connect: canceled (%v)\n", ctx.Err())
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded):
			fmt.Fprintf(stderr, "ts-unplug-proxy connect: %v (timed out after %s; proxy may still be connecting or waiting for Tailscale login)\n", err, *timeout)
		default:
			fmt.Fprintf(stderr, "ts-unplug-proxy connect: %v\n", err)
		}
		return 1
	}
	defer conn.Close()

	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}

	if *fdpass {
		if err := passConnFD(fdpassFD, conn); err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy connect: %v\n", err)
			return 1
		}
		return 0
	}

	// Restore default OS SIGINT/SIGTERM handling before entering the blocking
	// stdio relay so Ctrl-C / kill terminates immediately instead of hanging.
	stop()

	if err := relayStdio(conn, stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy connect: %v\n", err)
		return 1
	}
	return 0
}

// parseTargetArgs accepts either "<host> <port>" (OpenSSH %h %p) or "<host>:<port>".
func parseTargetArgs(args []string) (host string, port uint16, err error) {
	var rawHost, rawPort string
	switch len(args) {
	case 1:
		rawHost, rawPort, err = net.SplitHostPort(args[0])
		if err != nil {
			return "", 0, fmt.Errorf("invalid target %q: expected <host> <port> or <host>:<port>", args[0])
		}
	case 2:
		rawHost, rawPort = args[0], args[1]
	default:
		return "", 0, fmt.Errorf("expected <host> <port> or <host>:<port>, got %d argument(s)", len(args))
	}

	host = strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(rawHost), "]"), "[")
	if host == "" {
		return "", 0, errors.New("target host must not be empty")
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return "", 0, fmt.Errorf("target host %q contains invalid whitespace", host)
	}

	p, err := strconv.ParseUint(strings.TrimSpace(rawPort), 10, 16)
	if err != nil || p == 0 {
		return "", 0, fmt.Errorf("invalid target port %q: must be 1..65535", rawPort)
	}
	return host, uint16(p), nil
}

// checkStatusFastFail queries the optional /status endpoint and fails
// immediately when tsnet is blocked on login or explicitly stopped. Transient
// startup states ("Starting", "NoState") or unreachable status endpoints are
// ignored so normal connection queueing and proxy dial errors still run.
func checkStatusFastFail(ctx context.Context, statusAddr string) error {
	url := buildStatusURL(statusAddr)
	checkCtx, cancel := context.WithTimeout(ctx, statusCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Debug("status pre-check skipped", slog.String("url", url), slog.Any("error", err))
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}

	var body struct {
		Ready        bool   `json:"ready"`
		BackendState string `json:"backend_state"`
		AuthURL      string `json:"auth_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16*1024)).Decode(&body); err != nil {
		return nil
	}
	switch body.BackendState {
	case "NeedsLogin":
		if body.AuthURL != "" {
			return fmt.Errorf("proxy needs Tailscale login: %s", body.AuthURL)
		}
		return errors.New("proxy needs Tailscale login (check proxy logs or /status for auth URL)")
	case "NeedsMachineAuth":
		return errors.New("proxy node is awaiting Tailscale admin approval (NeedsMachineAuth)")
	case "Stopped":
		return errors.New("proxy Tailscale backend is stopped")
	default:
		return nil
	}
}

func buildStatusURL(addr string) string {
	addr = strings.TrimSpace(addr)
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		trimmed := strings.TrimRight(addr, "/")
		if strings.HasSuffix(trimmed, "/status") {
			return trimmed
		}
		return trimmed + "/status"
	}
	return "http://" + addr + "/status"
}

// dialSOCKS5 connects to a SOCKS5 proxy (no-auth, RFC 1928) and tunnels to
// host:port using remote hostname resolution (ATYP=0x03) for domain names.
//
// Reads use exact io.ReadFull counts directly on conn with zero buffering so
// no bytes of an immediate remote banner (e.g. SSH-2.0-...) are ever consumed
// ahead of FD passing or stdio relay.
func dialSOCKS5(ctx context.Context, proxyAddr, host string, port uint16) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("connect to SOCKS5 proxy %s (is ts-unplug-proxy running?): %w", proxyAddr, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()

	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set SOCKS5 handshake deadline: %w", err)
		}
	}

	// Cancel blocked handshake I/O promptly if ctx is canceled before deadline.
	stopCancel := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stopCancel()

	// 1. Greeting: VER=5, NMETHODS=1, METHOD=0 (no auth).
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return nil, fmt.Errorf("SOCKS5 greeting write: %w", err)
	}
	var greetResp [2]byte
	if _, err := io.ReadFull(conn, greetResp[:]); err != nil {
		return nil, fmt.Errorf("SOCKS5 greeting read: %w", err)
	}
	if greetResp[0] != 0x05 {
		return nil, fmt.Errorf("SOCKS5 proxy %s replied with unsupported version %d", proxyAddr, greetResp[0])
	}
	if greetResp[1] != 0x00 {
		return nil, fmt.Errorf("SOCKS5 proxy %s rejected no-auth method (method=0x%02x)", proxyAddr, greetResp[1])
	}

	// 2. CONNECT request.
	req, err := buildSOCKS5ConnectRequest(host, port)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(req); err != nil {
		return nil, fmt.Errorf("SOCKS5 connect write: %w", err)
	}

	// 3. Reply header: VER, REP, RSV, ATYP.
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, fmt.Errorf("SOCKS5 connect reply read: %w", err)
	}
	if hdr[0] != 0x05 {
		return nil, fmt.Errorf("SOCKS5 connect reply had invalid version %d", hdr[0])
	}
	if hdr[1] != 0x00 {
		return nil, fmt.Errorf("SOCKS5 connect to %s:%d failed: %s", host, port, socks5ReplyString(hdr[1]))
	}

	// Drain exact BND.ADDR + BND.PORT bytes with zero over-read.
	switch hdr[3] {
	case 0x01: // IPv4 + 2-byte port
		var tail [4 + 2]byte
		if _, err := io.ReadFull(conn, tail[:]); err != nil {
			return nil, fmt.Errorf("SOCKS5 IPv4 bind address read: %w", err)
		}
	case 0x04: // IPv6 + 2-byte port
		var tail [16 + 2]byte
		if _, err := io.ReadFull(conn, tail[:]); err != nil {
			return nil, fmt.Errorf("SOCKS5 IPv6 bind address read: %w", err)
		}
	case 0x03: // 1-byte length + domain + 2-byte port
		var lenBuf [1]byte
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			return nil, fmt.Errorf("SOCKS5 domain bind length read: %w", err)
		}
		tail := make([]byte, int(lenBuf[0])+2)
		if _, err := io.ReadFull(conn, tail); err != nil {
			return nil, fmt.Errorf("SOCKS5 domain bind address read: %w", err)
		}
	default:
		return nil, fmt.Errorf("SOCKS5 connect reply had unsupported address type 0x%02x", hdr[3])
	}

	// Disarm cancellation hook before clearing deadline so a concurrent cancel
	// cannot race with clearing the deadline.
	if !stopCancel() && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear SOCKS5 handshake deadline: %w", err)
	}

	ok = true
	return conn, nil
}

func buildSOCKS5ConnectRequest(host string, port uint16) ([]byte, error) {
	buf := make([]byte, 0, 7+len(host))
	buf = append(buf, 0x05, 0x01, 0x00) // VER=5, CMD=CONNECT, RSV=0

	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return nil, fmt.Errorf("SOCKS5 target IP %q must not include a zone", host)
		}
		ip = ip.Unmap()
		if ip.Is4() {
			a4 := ip.As4()
			buf = append(buf, 0x01)
			buf = append(buf, a4[:]...)
		} else {
			a16 := ip.As16()
			buf = append(buf, 0x04)
			buf = append(buf, a16[:]...)
		}
	} else {
		if len(host) > 255 {
			return nil, fmt.Errorf("target hostname %q exceeds SOCKS5 255-byte limit", host)
		}
		buf = append(buf, 0x03, byte(len(host)))
		buf = append(buf, host...)
	}

	buf = binary.BigEndian.AppendUint16(buf, port)
	return buf, nil
}

func socks5ReplyString(code byte) string {
	switch code {
	case 0x01:
		return "general SOCKS server failure (check tailnet reachability or proxy logs)"
	case 0x02:
		return "connection not allowed by ruleset"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	default:
		return fmt.Sprintf("unknown error 0x%02x", code)
	}
}

// dialHTTPConnect opens an HTTP/1.1 CONNECT tunnel to host:port through
// proxyAddr. Response headers are read byte-by-byte up to \r\n\r\n with zero
// buffering so no tunneled bytes are consumed ahead of FD passing or relay.
func dialHTTPConnect(ctx context.Context, proxyAddr, host string, port uint16) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("connect to HTTP proxy %s (is ts-unplug-proxy running?): %w", proxyAddr, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()

	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set HTTP CONNECT deadline: %w", err)
		}
	}

	stopCancel := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stopCancel()

	target := net.JoinHostPort(host, strconv.Itoa(int(port)))
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, fmt.Errorf("HTTP CONNECT write: %w", err)
	}

	rawHeaders, err := readHTTPHeadersExact(conn, maxHTTPConnectHeaderBytes)
	if err != nil {
		return nil, fmt.Errorf("HTTP CONNECT response read: %w", err)
	}

	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(rawHeaders)))
	statusLine, err := tp.ReadLine()
	if err != nil {
		return nil, fmt.Errorf("HTTP CONNECT invalid status line: %w", err)
	}
	headers, err := tp.ReadMIMEHeader()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("HTTP CONNECT invalid response headers: %w", err)
	}

	statusCode, err := parseHTTPStatusCode(statusLine)
	if err != nil {
		return nil, fmt.Errorf("HTTP CONNECT %w", err)
	}
	if statusCode < 200 || statusCode >= 300 {
		if detail := headers.Get("Tailscale-Connect-Error"); detail != "" {
			return nil, fmt.Errorf("HTTP CONNECT to %s failed (%s): %s", target, statusLine, detail)
		}
		return nil, fmt.Errorf("HTTP CONNECT to %s failed: %s", target, statusLine)
	}

	if !stopCancel() && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear HTTP CONNECT deadline: %w", err)
	}

	ok = true
	return conn, nil
}

// readHTTPHeadersExact reads one byte at a time until "\r\n\r\n" (or "\n\n"
// for non-compliant proxies), stopping at the exact header terminator so no
// tunneled payload bytes are ever over-read.
func readHTTPHeadersExact(r io.Reader, maxBytes int) ([]byte, error) {
	buf := make([]byte, 0, 256)
	var b [1]byte
	for len(buf) < maxBytes {
		n, err := r.Read(b[:])
		if n == 1 {
			buf = append(buf, b[0])
			if (len(buf) >= 4 && bytes.Equal(buf[len(buf)-4:], []byte("\r\n\r\n"))) ||
				(len(buf) >= 2 && bytes.Equal(buf[len(buf)-2:], []byte("\n\n"))) {
				return buf, nil
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("response headers exceeded %d bytes", maxBytes)
}

func parseHTTPStatusCode(statusLine string) (int, error) {
	proto, rest, ok := strings.Cut(statusLine, " ")
	if !ok || !strings.HasPrefix(proto, "HTTP/") {
		return 0, fmt.Errorf("malformed status line %q", statusLine)
	}
	codeStr, _, _ := strings.Cut(strings.TrimLeft(rest, " "), " ")
	code, err := strconv.Atoi(codeStr)
	if err != nil || code < 100 || code > 999 {
		return 0, fmt.Errorf("malformed status code in %q", statusLine)
	}
	return code, nil
}

// relayStdio copies between in/out and conn with SSH-oriented half-close
// semantics: when in reaches EOF, conn's write side is half-closed so the
// remote peer sees EOF while its remaining output finishes draining to out;
// when conn reaches EOF (remote closed), relayStdio closes conn and returns
// promptly without blocking on an open stdin.
func relayStdio(conn net.Conn, in io.Reader, out io.Writer) error {
	inDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, in)
		if err == nil {
			if hc, ok := conn.(halfCloser); ok {
				_ = hc.CloseWrite()
			}
		} else {
			_ = conn.Close()
		}
		inDone <- err
	}()

	_, outErr := io.Copy(out, conn)
	_ = conn.Close()

	// net.ErrClosed on our read means the stdin goroutine closed conn after
	// hitting an error; wait for it so the stdin error is never dropped.
	if errors.Is(outErr, net.ErrClosed) {
		if err := <-inDone; err != nil {
			return err
		}
		return nil
	}
	return outErr
}
