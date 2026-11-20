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
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/dnstype"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
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

func makePrefixView(ps ...string) *views.Slice[netip.Prefix] {
	list := make([]netip.Prefix, len(ps))
	for i, s := range ps {
		list[i] = netip.MustParsePrefix(s)
	}
	v := views.SliceOf(list)
	return &v
}

func makeDNSResponse(t *testing.T, name string, ips []netip.Addr) []byte {
	t.Helper()
	var answers []dnsmessage.Resource
	for _, ip := range ips {
		ip = ip.Unmap()
		if ip.Is4() {
			answers = append(answers, dnsmessage.Resource{
				Header: dnsmessage.ResourceHeader{
					Name:  dnsmessage.MustNewName(name),
					Type:  dnsmessage.TypeA,
					Class: dnsmessage.ClassINET,
					TTL:   60,
				},
				Body: &dnsmessage.AResource{A: ip.As4()},
			})
		} else {
			answers = append(answers, dnsmessage.Resource{
				Header: dnsmessage.ResourceHeader{
					Name:  dnsmessage.MustNewName(name),
					Type:  dnsmessage.TypeAAAA,
					Class: dnsmessage.ClassINET,
					TTL:   60,
				},
				Body: &dnsmessage.AAAAResource{AAAA: ip.As16()},
			})
		}
	}
	msg := dnsmessage.Message{
		Header:  dnsmessage.Header{Response: true, RCode: dnsmessage.RCodeSuccess},
		Answers: answers,
	}
	packed, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack DNS response: %v", err)
	}
	return packed
}

func TestValidateFlagCombos(t *testing.T) {
	if err := validateFlagCombos(false, true, true); err != nil {
		t.Errorf("without -tailnet-only should allow exit-node/direct: %v", err)
	}
	if err := validateFlagCombos(true, false, false); err != nil {
		t.Errorf("-tailnet-only alone should succeed: %v", err)
	}
	if err := validateFlagCombos(true, true, false); err == nil {
		t.Error("-tailnet-only + -exit-node succeeded, want error")
	}
	if err := validateFlagCombos(true, false, true); err == nil {
		t.Error("-tailnet-only + -direct succeeded, want error")
	}
}

func TestIsAllowedTailnetIP(t *testing.T) {
	peerWithSubnet := &ipnstate.Status{
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			key.NewNode().Public(): {
				HostName:      "router",
				PrimaryRoutes: makePrefixView("10.20.0.0/16", "fd00:abcd::/48"),
			},
			key.NewNode().Public(): {
				HostName:      "exit",
				PrimaryRoutes: makePrefixView("0.0.0.0/0", "::/0"),
			},
		},
	}

	cases := []struct {
		name         string
		ip           string
		status       *ipnstate.Status
		acceptRoutes bool
		want         bool
	}{
		{"cgnat_low", "100.64.0.1", nil, false, true},
		{"cgnat_high", "100.127.255.254", nil, false, true},
		{"tailnet_v6", "fd7a:115c:a1e0::1", nil, false, true},
		{"v4_mapped_cgnat", "::ffff:100.64.1.2", nil, false, true},
		{"v4_mapped_public", "::ffff:1.1.1.1", peerWithSubnet, true, false},
		{"public_v4_no_routes", "1.1.1.1", peerWithSubnet, false, false},
		{"public_v4_default_route_still_blocked", "1.1.1.1", peerWithSubnet, true, false},
		{"public_v6_default_route_still_blocked", "2606:4700::1", peerWithSubnet, true, false},
		{"cgnat_boundary_below", "100.63.255.255", peerWithSubnet, true, false},
		{"cgnat_boundary_above", "100.128.0.1", peerWithSubnet, true, false},
		{"peer_primary_route_blocked_without_accept_routes", "10.20.1.5", peerWithSubnet, false, false},
		{"peer_primary_route_allowed_with_accept_routes", "10.20.1.5", peerWithSubnet, true, true},
		{"peer_primary_route_v4_mapped_allowed", "::ffff:10.20.1.5", peerWithSubnet, true, true},
		{"peer_primary_route_v6_allowed", "fd00:abcd::42", peerWithSubnet, true, true},
		{"peer_primary_route_nil_status_fails_closed", "10.20.1.5", nil, true, false},
		{"unrouted_private_ip_blocked", "192.168.1.1", peerWithSubnet, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := netip.MustParseAddr(tc.ip)
			got := isAllowedTailnetIP(ip, tc.status, tc.acceptRoutes)
			if got != tc.want {
				t.Errorf("isAllowedTailnetIP(%q, acceptRoutes=%v) = %v, want %v", tc.ip, tc.acceptRoutes, got, tc.want)
			}
		})
	}

	if isAllowedTailnetIP(netip.Addr{}, peerWithSubnet, true) {
		t.Error("invalid netip.Addr allowed, want false")
	}
}

func TestTailnetDialerTailnetOnlyEnforcement(t *testing.T) {
	statusWithRoutes := &ipnstate.Status{
		MagicDNSSuffix: "wild-dojo.ts.net",
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			key.NewNode().Public(): {
				HostName:      "db",
				DNSName:       "db.wild-dojo.ts.net.",
				TailscaleIPs:  []netip.Addr{netip.MustParseAddr("100.64.2.10")},
				PrimaryRoutes: makePrefixView("10.50.0.0/16", "0.0.0.0/0"),
			},
		},
	}

	newDialer := func(acceptRoutes bool, dnsMap map[string][]netip.Addr, dnsErr error, status *ipnstate.Status, statusErr error) (*tailnetDialer, *string) {
		var dialed string
		d := &tailnetDialer{
			tailnetOnly:  true,
			acceptRoutes: acceptRoutes,
			statusFn: func(context.Context) (*ipnstate.Status, error) {
				return status, statusErr
			},
			queryDNSFn: func(_ context.Context, name, qtype string) ([]byte, []*dnstype.Resolver, error) {
				if dnsErr != nil {
					return nil, nil, dnsErr
				}
				ips := dnsMap[dnsMapKey(name)]
				var matching []netip.Addr
				for _, ip := range ips {
					if (qtype == "A" && ip.Unmap().Is4()) || (qtype == "AAAA" && ip.Unmap().Is6()) {
						matching = append(matching, ip)
					}
				}
				return makeDNSResponse(t, name, matching), nil, nil
			},
			dialTSFn: func(_ context.Context, _, addr string) (net.Conn, error) {
				dialed = addr
				c, s := net.Pipe()
				_ = s.Close()
				return c, nil
			},
		}
		return d, &dialed
	}

	t.Run("literal_cgnat_allowed", func(t *testing.T) {
		d, dialed := newDialer(false, nil, nil, nil, errors.New("offline"))
		c, err := d.Dial(context.Background(), "tcp", "100.64.1.2:443")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = c.Close()
		if *dialed != "100.64.1.2:443" {
			t.Errorf("dialed = %q, want 100.64.1.2:443", *dialed)
		}
	})

	t.Run("literal_v4_mapped_cgnat_allowed_normalized", func(t *testing.T) {
		d, dialed := newDialer(false, nil, nil, nil, nil)
		c, err := d.Dial(context.Background(), "tcp", "[::ffff:100.64.1.2]:80")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = c.Close()
		if *dialed != "100.64.1.2:80" {
			t.Errorf("dialed = %q, want 100.64.1.2:80", *dialed)
		}
	})

	t.Run("literal_v4_mapped_public_blocked", func(t *testing.T) {
		d, dialed := newDialer(true, nil, nil, statusWithRoutes, nil)
		_, err := d.Dial(context.Background(), "tcp", "[::ffff:1.1.1.1]:443")
		if err == nil || !strings.Contains(err.Error(), "blocked by -tailnet-only") {
			t.Fatalf("err = %v, want blocked by -tailnet-only", err)
		}
		if *dialed != "" {
			t.Errorf("dialTS called with %q despite block", *dialed)
		}
	})

	t.Run("literal_peer_route_requires_accept_routes", func(t *testing.T) {
		dNo, _ := newDialer(false, nil, nil, statusWithRoutes, nil)
		if _, err := dNo.Dial(context.Background(), "tcp", "10.50.1.9:80"); err == nil {
			t.Fatal("10.50.1.9 allowed without -accept-routes")
		}
		dYes, dialed := newDialer(true, nil, nil, statusWithRoutes, nil)
		c, err := dYes.Dial(context.Background(), "tcp", "10.50.1.9:80")
		if err != nil {
			t.Fatalf("10.50.1.9 with -accept-routes failed: %v", err)
		}
		_ = c.Close()
		if *dialed != "10.50.1.9:80" {
			t.Errorf("dialed = %q, want 10.50.1.9:80", *dialed)
		}
	})

	t.Run("literal_peer_route_nil_status_fails_closed", func(t *testing.T) {
		d, dialed := newDialer(true, nil, nil, nil, errors.New("status error"))
		if _, err := d.Dial(context.Background(), "tcp", "10.50.1.9:80"); err == nil {
			t.Fatal("10.50.1.9 allowed with nil status")
		}
		if *dialed != "" {
			t.Errorf("dialTS unexpectedly called with %q", *dialed)
		}
	})

	t.Run("domain_resolving_to_public_ip_blocked", func(t *testing.T) {
		dnsMap := map[string][]netip.Addr{
			"evil.example.com": {netip.MustParseAddr("1.1.1.1")},
		}
		d, dialed := newDialer(true, dnsMap, nil, statusWithRoutes, nil)
		_, err := d.Dial(context.Background(), "tcp", "evil.example.com:443")
		if err == nil || !strings.Contains(err.Error(), "blocked by -tailnet-only") {
			t.Fatalf("err = %v, want blocked by -tailnet-only", err)
		}
		if *dialed != "" {
			t.Errorf("dialTS called with %q despite block", *dialed)
		}
	})

	t.Run("domain_with_mixed_ips_dials_only_allowed_ip", func(t *testing.T) {
		dnsMap := map[string][]netip.Addr{
			"dual.example.com": {
				netip.MustParseAddr("1.1.1.1"),
				netip.MustParseAddr("100.64.9.9"),
			},
		}
		d, dialed := newDialer(false, dnsMap, nil, statusWithRoutes, nil)
		c, err := d.Dial(context.Background(), "tcp", "dual.example.com:443")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = c.Close()
		if *dialed != "100.64.9.9:443" {
			t.Errorf("dialed = %q, want 100.64.9.9:443", *dialed)
		}
	})

	t.Run("unresolved_hostname_blocked_never_falls_back", func(t *testing.T) {
		d, dialed := newDialer(false, map[string][]netip.Addr{}, nil, statusWithRoutes, nil)
		_, err := d.Dial(context.Background(), "tcp", "unknown.example.com:443")
		if err == nil || !strings.Contains(err.Error(), "blocked by -tailnet-only") {
			t.Fatalf("err = %v, want blocked by -tailnet-only", err)
		}
		if *dialed != "" {
			t.Errorf("dialTS unexpectedly called with %q on unresolved hostname", *dialed)
		}
	})

	t.Run("dns_error_blocked_never_falls_back", func(t *testing.T) {
		d, dialed := newDialer(false, nil, errors.New("dns down"), statusWithRoutes, nil)
		_, err := d.Dial(context.Background(), "tcp", "example.com:443")
		if err == nil || !strings.Contains(err.Error(), "blocked by -tailnet-only") {
			t.Fatalf("err = %v, want blocked by -tailnet-only", err)
		}
		if *dialed != "" {
			t.Errorf("dialTS unexpectedly called with %q on DNS error", *dialed)
		}
	})

	t.Run("magicdns_peer_fallback_allowed", func(t *testing.T) {
		d, dialed := newDialer(false, map[string][]netip.Addr{}, nil, statusWithRoutes, nil)
		c, err := d.Dial(context.Background(), "tcp", "db.wild-dojo.ts.net:5432")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = c.Close()
		if *dialed != "100.64.2.10:5432" {
			t.Errorf("dialed = %q, want 100.64.2.10:5432", *dialed)
		}
	})
}

func TestTailnetDialerSuffixesConcurrentRace(t *testing.T) {
	d := &tailnetDialer{suffixes: []string{"initial.ts.net"}}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = d.isStrictTailnetHost(nil, "foo.wild-dojo.ts.net")
			}
		}()
	}
	for range 50 {
		d.setSuffixes([]string{"wild-dojo.ts.net", "extra.internal"})
	}
	wg.Wait()
	if !d.isStrictTailnetHost(nil, "app.extra.internal") {
		t.Error("expected app.extra.internal to match updated suffix")
	}
}

func TestDoctorCheckPACFreshDirNoDomainWarnsNotFails(t *testing.T) {
	d := &doctor{
		dir:      t.TempDir(),
		httpAddr: "127.0.0.1:8080",
		pacAddr:  "127.0.0.1:8099",
	}
	d.checkPAC()
	if d.problems != 0 {
		t.Errorf("doctor checkPAC problems = %d, want 0 on fresh dir", d.problems)
	}
	if d.warnings == 0 {
		t.Errorf("doctor checkPAC warnings = 0, want >= 1 warning about dynamic MagicDNS discovery")
	}
}

func TestDoctorCheckListenersIncludesForwards(t *testing.T) {
	d := &doctor{
		forwards: forwardList{{local: "127.0.0.1:7687", remote: "neo4j.ts.net:7687"}},
	}
	d.checkListeners()
	if d.problems != 0 {
		t.Errorf("loopback -L-only problems = %d, want 0", d.problems)
	}

	dBad := &doctor{
		forwards: forwardList{{local: "0.0.0.0:7687", remote: "neo4j.ts.net:7687"}},
	}
	dBad.checkListeners()
	if dBad.problems == 0 {
		t.Error("non-loopback -L without -allow-non-loopback should fail")
	}

	// Offline doctor without listener flags or -pac-addr should not fail.
	dOffline := &doctor{}
	dOffline.checkListeners()
	if dOffline.problems != 0 {
		t.Errorf("offline doctor with no listener flags: problems = %d, want 0", dOffline.problems)
	}

	// -pac-addr alone with no proxy listener must still fail.
	dPACOnly := &doctor{pacAddr: "127.0.0.1:8099"}
	dPACOnly.checkListeners()
	if dPACOnly.problems == 0 {
		t.Error("doctor with -pac-addr only should fail")
	}
}

func TestDoctorPopulateFromRunningDaemon(t *testing.T) {
	sockPath := shortSockPath(t, "doc.sock")
	ln, err := listenControlSocket(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	st := &statusState{
		hostname:    "doc-proxy",
		socks5Addr:  "127.0.0.1:1080",
		httpAddr:    "127.0.0.1:8080",
		controlSock: sockPath,
		forwards:    []string{"127.0.0.1:7687=neo4j.wild-dojo.ts.net:7687"},
		suffixes:    []string{"wild-dojo.ts.net"},
		proxies:     []string{"PROXY 127.0.0.1:8080", "SOCKS5 127.0.0.1:1080"},
	}
	st.setServing()
	srv := &http.Server{Handler: newControlMux(nil, st, newConnTracker())}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	d := &doctor{controlSock: sockPath}
	d.populateFromRunningDaemon()
	if d.discoveredLive != sockPath {
		t.Fatalf("discoveredLive = %q, want %q", d.discoveredLive, sockPath)
	}
	if d.socksAddr != "127.0.0.1:1080" || d.httpAddr != "127.0.0.1:8080" {
		t.Errorf("discovered socks/http = %q/%q, want 127.0.0.1:1080/127.0.0.1:8080", d.socksAddr, d.httpAddr)
	}
	if len(d.forwards) != 1 || d.forwards[0].local != "127.0.0.1:7687" {
		t.Errorf("discovered forwards = %+v, want 127.0.0.1:7687", d.forwards)
	}
	d.checkListeners()
	if d.problems != 0 {
		t.Errorf("checkListeners on discovered live listeners: problems = %d, want 0", d.problems)
	}
}
