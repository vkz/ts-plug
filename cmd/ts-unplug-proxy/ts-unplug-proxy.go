package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/netns"
	"tailscale.com/net/proxymux"
	"tailscale.com/net/socks5"
	"tailscale.com/tsnet"
)

const tsnetHostname = "tsunplug-proxy"

var (
	flagDir                     = flag.String("dir", "", "tsnet server directory")
	flagVerboseProxy            = flag.Bool("v", false, "log proxy connections")
	flagVerboseTSNet            = flag.Bool("vv", false, "log proxy connections and tsnet debug info")
	flagSOCKS5Addr              = flag.String("socks5", "", "SOCKS5 proxy listen address")
	flagHTTPAddr                = flag.String("http", "", "HTTP proxy listen address")
	flagPACAddr                 = flag.String("pac-addr", "", "serve /proxy.pac on this listen address")
	flagPACDomains              = flag.String("pac-domain", "", "comma-separated tailnet DNS suffixes the PAC must never send DIRECT (default: MagicDNS name from -dir)")
	flagExitNode                = flag.Bool("exit-node", false, "use a tailnet exit node for non-tailnet egress (off by default)")
	flagClearExitNode           = flag.Bool("clear-exit-node", false, "one-shot: clear a persisted exit-node preference from -dir, then exit")
	flagDisableInterfaceBinding = flag.Bool("disable-interface-binding", false, "disable Tailscale's macOS network namespace integration")
	flagAcceptRoutes            = flag.Bool("accept-routes", false, "accept advertised subnet and Service routes")
	flagDirect                  = flag.Bool("direct", false, "force non-tailnet names through the OS resolver/dialer (rarely needed; off by default)")
	flagAllowNonLoopback        = flag.Bool("allow-non-loopback", false, "allow -socks5/-http/-pac-addr to bind non-loopback addresses")
	flagVersion                 = flag.Bool("version", false, "print version and build information, then exit")
)

type serveResult struct {
	name string
	err  error
}

type tailnetDialer struct {
	ts       *tsnet.Server
	lc       *local.Client
	direct   bool
	suffixes []string
}

type loggingListener struct {
	net.Listener
	proxy string
}

func (ln loggingListener) Accept() (net.Conn, error) {
	conn, err := ln.Listener.Accept()
	if err != nil {
		return nil, err
	}
	slog.Debug("proxy connection accepted",
		slog.String("proxy", ln.proxy),
		slog.String("local", conn.LocalAddr().String()),
		slog.String("remote", conn.RemoteAddr().String()),
	)
	return conn, nil
}

func main() {
	doctorMode := false
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		doctorMode = true
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}
	flag.Usage = printUsage
	flag.Parse()

	if *flagVersion {
		printVersion()
		return
	}
	if *flagDir == "" {
		slog.Error("-dir is required")
		os.Exit(1)
	}
	if flag.NArg() != 0 {
		slog.Error("positional remote addresses are not supported; proxy clients provide destinations")
		os.Exit(1)
	}
	if *flagClearExitNode {
		lock, err := lockDir(*flagDir)
		if err != nil {
			slog.Error("cannot clear exit-node preference", slog.Any("error", err))
			os.Exit(1)
		}
		defer lock.Close()
		if err := clearExitNode(*flagDir); err != nil {
			slog.Error("failed to clear exit-node preference", slog.Any("error", err))
			os.Exit(1)
		}
		return
	}
	if doctorMode {
		os.Exit(runDoctor())
	}
	if *flagSOCKS5Addr == "" && *flagHTTPAddr == "" {
		slog.Error("at least one of -socks5 or -http is required")
		os.Exit(1)
	}
	if err := checkLoopbackListeners(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
	pacDomains := splitCommaList(*flagPACDomains)
	if len(pacDomains) == 0 {
		if sf, err := loadStateFile(*flagDir); err == nil {
			if n := sf.magicDNSName(); n != "" {
				pacDomains = []string{n}
			}
		}
	}
	if *flagPACAddr != "" && len(pacDomains) == 0 {
		slog.Error("-pac-addr requires -pac-domain (could not derive a MagicDNS suffix from -dir)")
		os.Exit(1)
	}
	verboseProxy := *flagVerboseProxy || *flagVerboseTSNet
	if verboseProxy {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// One tsnet server per state dir: the state file has no cross-process lock.
	lock, err := lockDir(*flagDir)
	if err != nil {
		slog.Error("cannot use state dir", slog.Any("error", err))
		os.Exit(1)
	}
	defer lock.Close()

	serveErr := make(chan serveResult, 3)
	var closers []io.Closer
	closeAll := func() {
		for _, c := range closers {
			if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				slog.Warn("error closing listener", slog.Any("error", err))
			}
		}
	}
	defer closeAll()

	ts := &tsnet.Server{
		Hostname: tsnetHostname,
		Dir:      *flagDir,
	}
	if *flagVerboseTSNet {
		ts.Logf = func(format string, args ...any) {
			slog.Debug(fmt.Sprintf(format, args...))
		}
	}
	if *flagDisableInterfaceBinding {
		// tsnet's network namespace can bind outbound connections to the
		// macOS default interface. Disable it when another tunnel controls
		// the system's interface and route selection.
		netns.SetEnabled(false)
	}
	// Start the backend without waiting for a connection, so /status can report
	// progress (including the auth URL) while tsnet connects.
	lc, err := ts.LocalClient()
	if err != nil {
		slog.Error("failed to start tsnet backend", slog.Any("error", err))
		os.Exit(1)
	}

	// Bind listeners and serve the PAC before tsnet connects. The PAC is static,
	// so it is available immediately; tailnet requests then queue (fail closed)
	// while the node comes up instead of the browser falling back to DIRECT
	// because it could not fetch the PAC.
	status := &statusState{lc: lc, suffixes: pacDomains, proxies: pacProxies(*flagSOCKS5Addr, *flagHTTPAddr)}
	if *flagPACAddr != "" {
		listener, err := net.Listen("tcp", *flagPACAddr)
		if err != nil {
			slog.Error("failed to listen for PAC", slog.String("addr", *flagPACAddr), slog.Any("error", err))
			os.Exit(1)
		}
		closers = append(closers, listener)
		cfg := pacConfig{suffixes: pacDomains, proxies: status.proxies}
		startPACServer(listener, cfg.script(), status, verboseProxy, serveErr)
		slog.Info("PAC listening",
			slog.String("local", listener.Addr().String()),
			slog.String("url", "http://"+listener.Addr().String()+"/proxy.pac"),
			slog.String("suffixes", strings.Join(cfg.effectiveSuffixes(), ", ")),
		)
	}

	var socksListener, httpListener net.Listener
	if *flagSOCKS5Addr != "" && *flagSOCKS5Addr == *flagHTTPAddr {
		listener, err := net.Listen("tcp", *flagSOCKS5Addr)
		if err != nil {
			slog.Error("failed to listen", slog.String("addr", *flagSOCKS5Addr), slog.Any("error", err))
			os.Exit(1)
		}
		closers = append(closers, listener)
		socksListener, httpListener = proxymux.SplitSOCKSAndHTTP(listener)
	} else {
		if *flagSOCKS5Addr != "" {
			listener, err := net.Listen("tcp", *flagSOCKS5Addr)
			if err != nil {
				slog.Error("failed to listen for SOCKS5", slog.String("addr", *flagSOCKS5Addr), slog.Any("error", err))
				os.Exit(1)
			}
			closers = append(closers, listener)
			socksListener = listener
		}
		if *flagHTTPAddr != "" {
			listener, err := net.Listen("tcp", *flagHTTPAddr)
			if err != nil {
				slog.Error("failed to listen for HTTP proxy", slog.String("addr", *flagHTTPAddr), slog.Any("error", err))
				os.Exit(1)
			}
			closers = append(closers, listener)
			httpListener = listener
		}
	}

	upStatus, err := ts.Up(ctx)
	if err != nil {
		slog.Error("error starting tsnet server", slog.Any("error", err))
		os.Exit(1)
	}
	defer func() {
		if err := ts.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Warn("error closing tsnet server", slog.Any("error", err))
		}
	}()

	slog.Info("tsnet server started", slog.String("status", upStatus.BackendState))

	if *flagAcceptRoutes {
		if _, err := lc.EditPrefs(ctx, &ipn.MaskedPrefs{
			Prefs:       ipn.Prefs{RouteAll: true},
			RouteAllSet: true,
		}); err != nil {
			slog.Error("failed to enable advertised routes", slog.Any("error", err))
			os.Exit(1)
		}
		slog.Info("advertised subnet and Service routes enabled")
	}
	if *flagExitNode {
		if err := useExitNodeIfAvailable(ctx, lc); err != nil {
			slog.Warn("failed to configure tailnet exit node", slog.Any("error", err))
		}
	}
	dialer := (&tailnetDialer{ts: ts, lc: lc, direct: *flagDirect, suffixes: pacDomains}).Dial

	// Wait briefly for the network map so the first proxied dial does not fail
	// while tsnet is still applying it. The listeners are already bound and the
	// PAC is served; early connections queue in the backlog.
	waitForNetmap(ctx, lc, 10*time.Second)

	// tsnet is up; start serving the already-bound proxy listeners.
	if socksListener != nil {
		ln := socksListener
		if verboseProxy {
			ln = loggingListener{Listener: ln, proxy: "socks5"}
		}
		startSOCKS5Proxy(ln, dialer, serveErr)
		slog.Info("SOCKS5 proxy listening", slog.String("local", ln.Addr().String()))
	}
	if httpListener != nil {
		ln := httpListener
		if verboseProxy {
			ln = loggingListener{Listener: ln, proxy: "http"}
		}
		startHTTPProxy(ln, dialer, serveErr)
		slog.Info("HTTP proxy listening", slog.String("local", ln.Addr().String()))
	}
	select {
	case <-ctx.Done():
	case result := <-serveErr:
		if result.err != nil && !errors.Is(result.err, net.ErrClosed) && !errors.Is(result.err, http.ErrServerClosed) {
			slog.Error("proxy stopped with error", slog.String("proxy", result.name), slog.Any("error", result.err))
			os.Exit(1)
		}
	}
}

func useExitNodeIfAvailable(ctx context.Context, lc *local.Client) error {
	suggestion, err := lc.SuggestExitNode(ctx)
	if err != nil {
		return fmt.Errorf("suggest exit node: %w", err)
	}
	if suggestion.ID == "" {
		slog.Info("no tailnet exit node available")
		return nil
	}

	prefs, err := lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs:           ipn.Prefs{AutoExitNode: ipn.AnyExitNode},
		AutoExitNodeSet: true,
	})
	if err != nil {
		return fmt.Errorf("enable automatic exit node: %w", err)
	}

	slog.Info("automatic tailnet exit node enabled",
		slog.String("suggested_exit_node", string(suggestion.ID)),
		slog.String("suggested_exit_node_name", suggestion.Name),
		slog.String("selected_exit_node", string(prefs.ExitNodeID)),
	)
	return nil
}

func (d *tailnetDialer) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resolvedAddr, resolved, err := d.resolveAddr(dialCtx, network, addr)
	if err != nil {
		return nil, err
	}
	if !resolved {
		if d.direct {
			var nd net.Dialer
			slog.Debug("dialing directly (bypassing tailnet)",
				slog.String("network", network),
				slog.String("addr", addr),
			)
			return nd.DialContext(dialCtx, network, addr)
		}
		slog.Debug("dialing through tailnet with original host",
			slog.String("network", network),
			slog.String("addr", addr),
		)
		return d.ts.Dial(dialCtx, network, addr)
	}
	slog.Debug("dialing through tailnet",
		slog.String("network", network),
		slog.String("addr", addr),
		slog.String("resolved_addr", resolvedAddr),
	)
	return d.ts.Dial(dialCtx, network, resolvedAddr)
}

func (d *tailnetDialer) resolveAddr(ctx context.Context, network, addr string) (resolvedAddr string, resolved bool, err error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", false, err
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return net.JoinHostPort(ip.String(), port), true, nil
	}

	status, statusErr := d.lc.Status(ctx)
	if statusErr != nil {
		slog.Debug("tailnet status lookup failed", slog.String("host", host), slog.Any("error", statusErr))
	}

	// Query Tailscale DNS before using peer status aliases. A Tailscale Service
	// name can intentionally collide with a host name (for example, the
	// service "example" hosted by the peer "example"). Peer status then
	// contains the host IP, while Tailscale DNS contains the Service VIP.
	ips, lookupErr := d.lookupIP(ctx, network, host)
	err = lookupErr
	if len(ips) == 0 {
		ips = lookupMagicDNS(status, network, host)
		if len(ips) > 0 {
			err = nil
		}
	}
	if err != nil {
		if d.isStrictTailnetHost(status, host) {
			return "", false, fmt.Errorf("tailnet DNS lookup for %q: %w", host, err)
		}
		slog.Debug("tailnet DNS lookup failed; falling back to normal tsnet dial",
			slog.String("host", host),
			slog.Any("error", err),
		)
		return "", false, nil
	}
	if len(ips) == 0 {
		if d.isStrictTailnetHost(status, host) {
			return "", false, fmt.Errorf("tailnet DNS lookup for %q returned no addresses", host)
		}
		return "", false, nil
	}

	resolvedAddr = net.JoinHostPort(ips[0].String(), port)
	slog.Debug("tailnet DNS resolved",
		slog.String("host", host),
		slog.String("network", network),
		slog.String("resolved_addr", resolvedAddr),
	)
	return resolvedAddr, true, nil
}

func lookupMagicDNS(status *ipnstate.Status, network, host string) []netip.Addr {
	if status == nil {
		return nil
	}
	hostKey := dnsMapKey(host)
	var ips []netip.Addr
	addPeerIPs := func(peer *ipnstate.PeerStatus) {
		if peer == nil || !peerMatchesHost(status, peer, hostKey) {
			return
		}
		for _, ip := range peer.TailscaleIPs {
			if ipMatchesNetwork(network, ip) {
				ips = append(ips, ip)
			}
		}
	}

	addPeerIPs(status.Self)
	for _, peer := range status.Peer {
		addPeerIPs(peer)
	}
	if len(ips) == 0 {
		return nil
	}

	slog.Debug("tailnet status resolved",
		slog.String("host", host),
		slog.String("network", network),
		slog.String("ips", fmt.Sprint(ips)),
	)
	return ips
}

func peerMatchesHost(status *ipnstate.Status, peer *ipnstate.PeerStatus, hostKey string) bool {
	for _, name := range peerDNSNames(status, peer) {
		if dnsMapKey(name) == hostKey {
			return true
		}
	}
	return false
}

func peerDNSNames(status *ipnstate.Status, peer *ipnstate.PeerStatus) []string {
	var names []string
	if peer.DNSName != "" {
		names = append(names, peer.DNSName)
	}
	if peer.HostName != "" {
		names = append(names, peer.HostName)
	}

	for _, suffix := range magicDNSSuffixes(status) {
		for _, name := range slices.Clone(names) {
			name = strings.TrimSuffix(name, ".")
			if name == "" {
				continue
			}
			if strings.HasSuffix(dnsMapKey(name), "."+dnsMapKey(suffix)) {
				short := strings.TrimSuffix(name, "."+strings.TrimSuffix(suffix, "."))
				names = append(names, short)
			} else if peer.HostName != "" {
				names = append(names, strings.TrimSuffix(peer.HostName, ".")+"."+strings.TrimSuffix(suffix, "."))
			}
		}
	}

	return names
}

func magicDNSSuffixes(status *ipnstate.Status) []string {
	if status == nil {
		return nil
	}
	var suffixes []string
	if status.MagicDNSSuffix != "" {
		suffixes = append(suffixes, status.MagicDNSSuffix)
	}
	if status.CurrentTailnet != nil && status.CurrentTailnet.MagicDNSSuffix != "" {
		suffixes = append(suffixes, status.CurrentTailnet.MagicDNSSuffix)
	}
	return suffixes
}

func (d *tailnetDialer) isStrictTailnetHost(status *ipnstate.Status, host string) bool {
	hostKey := dnsMapKey(host)
	if !strings.Contains(hostKey, ".") {
		return true
	}
	suffixes := append(append([]string{}, magicDNSSuffixes(status)...), d.suffixes...)
	for _, suffix := range suffixes {
		suffixKey := dnsMapKey(suffix)
		if suffixKey != "" && (hostKey == suffixKey || strings.HasSuffix(hostKey, "."+suffixKey)) {
			return true
		}
	}
	return strings.HasSuffix(hostKey, ".ts.net")
}

func dnsMapKey(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

func ipMatchesNetwork(network string, ip netip.Addr) bool {
	switch {
	case strings.HasSuffix(network, "4"):
		return ip.Is4()
	case strings.HasSuffix(network, "6"):
		return ip.Is6()
	default:
		return true
	}
}

func (d *tailnetDialer) lookupIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	var ips []netip.Addr
	var errs []error
	for _, queryType := range queryTypesForNetwork(network) {
		response, resolvers, err := d.lc.QueryDNS(ctx, dnsQueryName(host), queryType)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s query: %w", queryType, err))
			continue
		}
		queryIPs, err := parseDNSIPs(response, queryType)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s response: %w", queryType, err))
			continue
		}
		slog.Debug("tailnet DNS query complete",
			slog.String("host", host),
			slog.String("type", queryType),
			slog.Int("answers", len(queryIPs)),
			slog.String("resolvers", fmt.Sprint(resolvers)),
		)
		ips = append(ips, queryIPs...)
	}
	if len(ips) > 0 {
		return ips, nil
	}
	return nil, errors.Join(errs...)
}

func dnsQueryName(host string) string {
	if strings.HasSuffix(host, ".") {
		return host
	}
	return host + "."
}

func queryTypesForNetwork(network string) []string {
	if strings.HasSuffix(network, "4") {
		return []string{"A"}
	}
	if strings.HasSuffix(network, "6") {
		return []string{"AAAA"}
	}
	return []string{"A", "AAAA"}
}

func parseDNSIPs(response []byte, queryType string) ([]netip.Addr, error) {
	var parser dnsmessage.Parser
	header, err := parser.Start(response)
	if err != nil {
		return nil, err
	}
	if header.RCode != dnsmessage.RCodeSuccess {
		return nil, fmt.Errorf("rcode %v", header.RCode)
	}
	if err := parser.SkipAllQuestions(); err != nil {
		return nil, err
	}

	var ips []netip.Addr
	for {
		answerHeader, err := parser.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			return ips, nil
		}
		if err != nil {
			return nil, err
		}

		switch {
		case queryType == "A" && answerHeader.Type == dnsmessage.TypeA:
			answer, err := parser.AResource()
			if err != nil {
				return nil, err
			}
			ips = append(ips, netip.AddrFrom4(answer.A))
		case queryType == "AAAA" && answerHeader.Type == dnsmessage.TypeAAAA:
			answer, err := parser.AAAAResource()
			if err != nil {
				return nil, err
			}
			ips = append(ips, netip.AddrFrom16(answer.AAAA))
		default:
			if err := parser.SkipAnswer(); err != nil {
				return nil, err
			}
		}
	}
}

func startSOCKS5Proxy(listener net.Listener, dialer func(context.Context, string, string) (net.Conn, error), serveErr chan<- serveResult) {
	proxy := &socks5.Server{
		Logf: func(format string, args ...any) {
			slog.Debug(fmt.Sprintf(format, args...))
		},
		Dialer: dialer,
	}
	go func() {
		serveErr <- serveResult{name: "socks5", err: proxy.Serve(listener)}
	}()
}

func startHTTPProxy(listener net.Listener, dialer func(context.Context, string, string) (net.Conn, error), serveErr chan<- serveResult) {
	server := &http.Server{
		Handler: httpProxyHandler(dialer),
	}
	go func() {
		serveErr <- serveResult{name: "http", err: server.Serve(listener)}
	}()
}

func httpProxyHandler(dialer func(context.Context, string, string) (net.Conn, error)) http.Handler {
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {},
		Transport: &http.Transport{
			DialContext: dialer,
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("HTTP proxy request", slog.String("method", r.Method), slog.String("target", r.RequestURI))

		if r.Method != http.MethodConnect {
			if strings.HasPrefix(r.RequestURI, "/") || r.RequestURI == "*" {
				http.Error(w, "bogus RequestURI; must be absolute URL or CONNECT", http.StatusBadRequest)
				return
			}
			proxy.ServeHTTP(w, r)
			return
		}

		dst := r.RequestURI
		backend, err := dialer(r.Context(), "tcp", dst)
		if err != nil {
			w.Header().Set("Tailscale-Connect-Error", err.Error())
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer backend.Close()

		client, clientBuf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer client.Close()

		if _, err := io.WriteString(client, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
			return
		}

		var clientSrc io.Reader = clientBuf
		if clientBuf.Reader.Buffered() == 0 {
			clientSrc = client
		}

		errc := make(chan error, 2)
		go func() {
			_, err := io.Copy(client, backend)
			errc <- err
		}()
		go func() {
			_, err := io.Copy(backend, clientSrc)
			errc <- err
		}()
		<-errc
	})
}

// splitCommaList splits a comma-separated flag value, dropping blanks.
func splitCommaList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isLoopbackListenAddr reports whether a listen address is restricted to the
// loopback interface. An empty host (":8080") means all interfaces and is not
// loopback.
func isLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.IsLoopback()
}

// checkLoopbackListeners refuses non-loopback listeners unless the operator
// opted in with -allow-non-loopback.
func checkLoopbackListeners() error {
	if *flagAllowNonLoopback {
		return nil
	}
	for _, l := range []struct{ flag, addr string }{
		{"-socks5", *flagSOCKS5Addr},
		{"-http", *flagHTTPAddr},
		{"-pac-addr", *flagPACAddr},
	} {
		if l.addr != "" && !isLoopbackListenAddr(l.addr) {
			return fmt.Errorf("refusing non-loopback listener %s %s without -allow-non-loopback", l.flag, l.addr)
		}
	}
	return nil
}

// clearExitNode clears a persisted exit-node preference from the state dir.
// The caller must hold the state-dir lock (i.e. the proxy is stopped).
func clearExitNode(dir string) error {
	sf, err := loadStateFile(dir)
	if err != nil {
		return err
	}
	changed, err := sf.clearExitNode()
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		slog.Info("no persisted exit-node preference to clear", slog.String("dir", dir))
		return nil
	}
	slog.Info("cleared persisted exit-node preference",
		slog.String("dir", dir),
		slog.String("fields", strings.Join(changed, ",")),
	)
	return nil
}
