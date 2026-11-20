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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/client/local"
	_ "tailscale.com/feature/taildrop"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/netns"
	"tailscale.com/net/proxymux"
	"tailscale.com/net/socks5"
	"tailscale.com/tsnet"
	"tailscale.com/types/dnstype"
)

const (
	tsnetHostname      = "tsunplug-proxy"
	defaultDialTimeout = 30 * time.Second
)

var (
	flagDir                     = flag.String("dir", "", "tsnet server directory")
	flagHostname                = flag.String("hostname", tsnetHostname, "tailnet hostname for the tsnet node")
	flagAdvertiseTags           = flag.String("advertise-tags", "", "comma-separated Tailscale ACL tags to advertise (e.g. tag:proxy)")
	flagDialTimeout             = flag.Duration("dial-timeout", defaultDialTimeout, "timeout for dialing destinations through tsnet")
	flagVerboseProxy            = flag.Bool("v", false, "log proxy connections")
	flagVerboseTSNet            = flag.Bool("vv", false, "log proxy connections and tsnet debug info")
	flagSOCKS5Addr              = flag.String("socks5", "", "SOCKS5 proxy listen address")
	flagHTTPAddr                = flag.String("http", "", "HTTP proxy listen address")
	flagPACAddr                 = flag.String("pac-addr", "", "serve /proxy.pac on this listen address")
	flagPACDomains              = flag.String("pac-domain", "", "comma-separated tailnet DNS suffixes the PAC must never send DIRECT (default: MagicDNS name from -dir or discovered at runtime)")
	flagControlSock             = flag.String("control-sock", "", "override path for the control Unix socket (default: <dir>/control.sock)")
	flagForwards                forwardList
	flagExitNode                = flag.Bool("exit-node", false, "use a tailnet exit node for non-tailnet egress (off by default)")
	flagClearExitNode           = flag.Bool("clear-exit-node", false, "one-shot: clear a persisted exit-node preference from -dir, then exit")
	flagDisableInterfaceBinding = flag.Bool("disable-interface-binding", false, "disable Tailscale's macOS network namespace integration")
	flagAcceptRoutes            = flag.Bool("accept-routes", false, "accept advertised subnet and Service routes")
	flagDirect                  = flag.Bool("direct", false, "force non-tailnet names through the OS resolver/dialer (rarely needed; off by default)")
	flagTailnetOnly             = flag.Bool("tailnet-only", false, "block destinations outside tailnet IPs (and non-default peer routes when -accept-routes is set)")
	flagAllowNonLoopback        = flag.Bool("allow-non-loopback", false, "allow -socks5/-http/-pac-addr/-L to bind non-loopback addresses")
	flagVersion                 = flag.Bool("version", false, "print version and build information, then exit")
)

func init() {
	flag.Var(&flagForwards, "L", "static local TCP port forward <local-addr>=<remote-host:port> (repeatable)")
}

type serveResult struct {
	name string
	err  error
}

type tailnetDialer struct {
	ts           *tsnet.Server
	lc           *local.Client
	direct       bool
	tailnetOnly  bool
	acceptRoutes bool
	suffixesMu   sync.RWMutex
	suffixes     []string
	dialTimeout  time.Duration

	// Optional hooks for unit testing without a live tsnet backend.
	statusFn   func(context.Context) (*ipnstate.Status, error)
	queryDNSFn func(context.Context, string, string) ([]byte, []*dnstype.Resolver, error)
	dialTSFn   func(context.Context, string, string) (net.Conn, error)
}

func (d *tailnetDialer) setSuffixes(suffixes []string) {
	d.suffixesMu.Lock()
	d.suffixes = slices.Clone(suffixes)
	d.suffixesMu.Unlock()
}

func (d *tailnetDialer) getSuffixes() []string {
	d.suffixesMu.RLock()
	out := slices.Clone(d.suffixes)
	d.suffixesMu.RUnlock()
	return out
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
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "connect":
			os.Exit(runConnect(os.Args[2:]))
		case "status":
			os.Exit(runStatus(os.Args[2:]))
		case "peers":
			os.Exit(runPeers(os.Args[2:]))
		case "ping":
			os.Exit(runPing(os.Args[2:]))
		case "login":
			os.Exit(runLogin(os.Args[2:]))
		case "conns":
			os.Exit(runConns(os.Args[2:]))
		case "env":
			os.Exit(runEnv(os.Args[2:]))
		case "file":
			os.Exit(runFile(os.Args[2:]))
		case "cp":
			os.Exit(runCP(os.Args[2:]))
		case "help":
			os.Exit(runHelp(os.Args[2:]))
		case "doctor":
			doctorMode = true
			os.Args = append(os.Args[:1], os.Args[2:]...)
		}
	}
	if doctorMode {
		flag.Usage = printDoctorUsage
	} else {
		flag.Usage = printUsage
	}
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
	hostname := strings.TrimSpace(*flagHostname)
	if hostname == "" {
		slog.Error("-hostname must not be empty")
		os.Exit(1)
	}
	advertiseTags, err := parseAdvertiseTags(*flagAdvertiseTags)
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
	if *flagDialTimeout <= 0 {
		slog.Error("-dial-timeout must be positive")
		os.Exit(1)
	}
	if err := validateFlagCombos(*flagTailnetOnly, *flagExitNode, *flagDirect); err != nil {
		slog.Error(err.Error())
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
	if *flagSOCKS5Addr == "" && *flagHTTPAddr == "" && len(flagForwards) == 0 {
		slog.Error("at least one of -socks5, -http, or -L is required")
		os.Exit(1)
	}
	if *flagPACAddr != "" && *flagSOCKS5Addr == "" && *flagHTTPAddr == "" {
		slog.Error("-pac-addr requires at least one of -socks5 or -http")
		os.Exit(1)
	}
	if err := checkLoopbackListeners(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}

	os.Exit(runDaemon(hostname, advertiseTags))
}

func runDaemon(hostname string, advertiseTags []string) int {
	explicitPACDomains := splitCommaList(*flagPACDomains)
	pacDomains := explicitPACDomains
	if len(pacDomains) == 0 {
		if sf, err := loadStateFile(*flagDir); err == nil {
			if n := sf.magicDNSName(); n != "" {
				pacDomains = []string{n}
			}
		}
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
		return 1
	}
	defer lock.Close()

	serveErr := make(chan serveResult, 4+len(flagForwards))
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
		Hostname:      hostname,
		Dir:           *flagDir,
		AdvertiseTags: advertiseTags,
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
	// Start the backend without waiting for a connection, so /status and the
	// control socket can report progress (including the auth URL) while tsnet connects.
	lc, err := ts.LocalClient()
	if err != nil {
		slog.Error("failed to start tsnet backend", slog.Any("error", err))
		return 1
	}
	defer func() {
		if err := ts.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Warn("error closing tsnet server", slog.Any("error", err))
		}
	}()

	tracker := newConnTracker()
	status := &statusState{
		lc:          lc,
		tracker:     tracker,
		hostname:    hostname,
		suffixes:    pacDomains,
		proxies:     pacProxies(*flagSOCKS5Addr, *flagHTTPAddr),
		socks5Addr:  *flagSOCKS5Addr,
		httpAddr:    *flagHTTPAddr,
		forwards:    flagForwards.strings(),
		tailnetOnly: *flagTailnetOnly,
	}

	// Start the Unix control socket immediately after lockDir and ts.LocalClient()
	// (BEFORE ts.Up) so CLI status/login work during NeedsLogin.
	controlSockPath := strings.TrimSpace(*flagControlSock)
	if controlSockPath == "" {
		controlSockPath = filepath.Join(*flagDir, "control.sock")
	}
	controlLn, err := listenControlSocket(controlSockPath)
	if err != nil {
		slog.Error("failed to listen on control socket", slog.String("socket", controlSockPath), slog.Any("error", err))
		return 1
	}
	if controlLn != nil {
		closers = append(closers, controlLn)
		status.setControlSock(controlSockPath)
		startControlServer(ctx, controlLn, lc.Dial, status, tracker, serveErr)
		slog.Info("control socket listening", slog.String("socket", controlSockPath))
	}

	// Bind listeners and serve the PAC before tsnet connects. Pre-Up the PAC
	// still fails closed on .ts.net, 100.64.0.0/10, and fd7a:115c:a1e0::/48;
	// after waitForNetmap we atomically swap in the discovered MagicDNS suffix.
	var pacHold *pacHolder
	if *flagPACAddr != "" {
		listener, err := net.Listen("tcp", *flagPACAddr)
		if err != nil {
			slog.Error("failed to listen for PAC", slog.String("addr", *flagPACAddr), slog.Any("error", err))
			return 1
		}
		closers = append(closers, listener)
		cfg := pacConfig{suffixes: pacDomains, proxies: status.proxies}
		pacHold = newPACHolder(cfg)
		includeAuthURL := isLoopbackListenAddr(*flagPACAddr)
		startPACServer(listener, pacHold, status, includeAuthURL, verboseProxy, serveErr)
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
			return 1
		}
		closers = append(closers, listener)
		socksListener, httpListener = proxymux.SplitSOCKSAndHTTP(listener)
	} else {
		if *flagSOCKS5Addr != "" {
			listener, err := net.Listen("tcp", *flagSOCKS5Addr)
			if err != nil {
				slog.Error("failed to listen for SOCKS5", slog.String("addr", *flagSOCKS5Addr), slog.Any("error", err))
				return 1
			}
			closers = append(closers, listener)
			socksListener = listener
		}
		if *flagHTTPAddr != "" {
			listener, err := net.Listen("tcp", *flagHTTPAddr)
			if err != nil {
				slog.Error("failed to listen for HTTP proxy", slog.String("addr", *flagHTTPAddr), slog.Any("error", err))
				return 1
			}
			closers = append(closers, listener)
			httpListener = listener
		}
	}

	// Bind all -L port-forward listeners before ts.Up (fail-closed startup).
	type boundForward struct {
		spec     forwardSpec
		listener net.Listener
	}
	boundForwards := make([]boundForward, 0, len(flagForwards))
	for _, spec := range flagForwards {
		ln, err := net.Listen("tcp", spec.local)
		if err != nil {
			slog.Error("failed to listen for port forward", slog.String("local", spec.local), slog.String("remote", spec.remote), slog.Any("error", err))
			return 1
		}
		closers = append(closers, ln)
		boundForwards = append(boundForwards, boundForward{spec: spec, listener: ln})
	}

	upStatus, err := ts.Up(ctx)
	if err != nil {
		slog.Error("error starting tsnet server", slog.Any("error", err))
		return 1
	}

	slog.Info("tsnet server started", slog.String("status", upStatus.BackendState))

	if *flagAcceptRoutes {
		if _, err := lc.EditPrefs(ctx, &ipn.MaskedPrefs{
			Prefs:       ipn.Prefs{RouteAll: true},
			RouteAllSet: true,
		}); err != nil {
			slog.Error("failed to enable advertised routes", slog.Any("error", err))
			return 1
		}
		slog.Info("advertised subnet and Service routes enabled")
	}
	if *flagExitNode {
		if err := useExitNodeIfAvailable(ctx, lc); err != nil {
			slog.Warn("failed to configure tailnet exit node", slog.Any("error", err))
		}
	}

	td := &tailnetDialer{
		ts:           ts,
		lc:           lc,
		direct:       *flagDirect,
		tailnetOnly:  *flagTailnetOnly,
		acceptRoutes: *flagAcceptRoutes,
		suffixes:     slices.Clone(pacDomains),
		dialTimeout:  *flagDialTimeout,
	}

	// Wait briefly for the network map so the first proxied dial does not fail
	// while tsnet is still applying it. The listeners are already bound and the
	// PAC is served; early connections queue in the backlog.
	st := waitForNetmap(ctx, lc, 10*time.Second)
	if st == nil || st.Self == nil || !st.Self.InNetworkMap {
		slog.Warn("network map not ready after 10s; starting proxy listeners anyway")
	}

	// Dynamically discover MagicDNS suffixes from live Status, merge with any
	// explicit -pac-domain suffixes, and atomically update PAC, status, and dialer.
	if liveSuffixes := magicDNSSuffixes(st); len(liveSuffixes) > 0 {
		merged := mergeSuffixes(explicitPACDomains, pacDomains, liveSuffixes)
		td.setSuffixes(merged)
		status.setSuffixes(merged)
		if pacHold != nil {
			newCfg := pacConfig{suffixes: merged, proxies: status.proxies}
			pacHold.Update(newCfg)
			slog.Info("PAC suffixes updated from live MagicDNS",
				slog.String("suffixes", strings.Join(newCfg.effectiveSuffixes(), ", ")),
			)
		}
	}

	// tsnet is up; start serving the already-bound proxy and forward listeners.
	if socksListener != nil {
		ln := socksListener
		if verboseProxy {
			ln = loggingListener{Listener: ln, proxy: "socks5"}
		}
		startSOCKS5Proxy(ctx, ln, tracker.WrapDial("socks5", td.Dial), serveErr)
		slog.Info("SOCKS5 proxy listening", slog.String("local", ln.Addr().String()))
	}
	if httpListener != nil {
		ln := httpListener
		if verboseProxy {
			ln = loggingListener{Listener: ln, proxy: "http"}
		}
		startHTTPProxy(ctx, ln, tracker.WrapDial("http", td.Dial), serveErr)
		slog.Info("HTTP proxy listening", slog.String("local", ln.Addr().String()))
	}
	forwardDial := tracker.WrapDial("forward", td.Dial)
	for _, bf := range boundForwards {
		startForward(ctx, bf.listener, bf.spec, forwardDial, verboseProxy, serveErr)
		slog.Info("TCP port forward listening",
			slog.String("local", bf.listener.Addr().String()),
			slog.String("remote", bf.spec.remote),
		)
	}
	status.setServing()

	select {
	case <-ctx.Done():
		return 0
	case result := <-serveErr:
		if result.err != nil && !errors.Is(result.err, net.ErrClosed) && !errors.Is(result.err, http.ErrServerClosed) {
			slog.Error("proxy stopped with error", slog.String("proxy", result.name), slog.Any("error", result.err))
			return 1
		}
		return 0
	}
}

func validateFlagCombos(tailnetOnly, exitNode, direct bool) error {
	if tailnetOnly && exitNode {
		return errors.New("-tailnet-only cannot be combined with -exit-node")
	}
	if tailnetOnly && direct {
		return errors.New("-tailnet-only cannot be combined with -direct")
	}
	return nil
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

func (d *tailnetDialer) timeout() time.Duration {
	if d.dialTimeout > 0 {
		return d.dialTimeout
	}
	return defaultDialTimeout
}

func (d *tailnetDialer) fetchStatus(ctx context.Context) (*ipnstate.Status, error) {
	if d.statusFn != nil {
		return d.statusFn(ctx)
	}
	if d.lc == nil {
		return nil, errors.New("no local client")
	}
	return d.lc.Status(ctx)
}

func (d *tailnetDialer) queryDNS(ctx context.Context, name, queryType string) ([]byte, []*dnstype.Resolver, error) {
	if d.queryDNSFn != nil {
		return d.queryDNSFn(ctx, name, queryType)
	}
	if d.lc == nil {
		return nil, nil, errors.New("no local client")
	}
	return d.lc.QueryDNS(ctx, name, queryType)
}

func (d *tailnetDialer) dialTS(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.dialTSFn != nil {
		return d.dialTSFn(ctx, network, addr)
	}
	return d.ts.Dial(ctx, network, addr)
}

func (d *tailnetDialer) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, d.timeout())
	defer cancel()

	resolvedAddr, resolved, err := d.resolveAddr(dialCtx, network, addr)
	if err != nil {
		return nil, err
	}
	if !resolved {
		if d.tailnetOnly {
			return nil, fmt.Errorf("destination %q blocked by -tailnet-only: unresolved host", addr)
		}
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
		return d.dialTS(dialCtx, network, addr)
	}
	slog.Debug("dialing through tailnet",
		slog.String("network", network),
		slog.String("addr", addr),
		slog.String("resolved_addr", resolvedAddr),
	)
	return d.dialTS(dialCtx, network, resolvedAddr)
}

// isAllowedTailnetIP reports whether ip is permitted under -tailnet-only.
// It normalizes IPv4-mapped IPv6 addresses via ip.Unmap() and allows:
//   - Tailscale IPv4 CGNAT space (100.64.0.0/10)
//   - Tailscale IPv6 space (fd7a:115c:a1e0::/48)
//   - Non-default peer PrimaryRoutes (p.Bits() > 0) when acceptRoutes is true
//
// Default routes (0.0.0.0/0 and ::/0) are explicitly excluded. When status is
// nil and ip is outside CGNAT/tailnet-v6, it fails closed (returns false).
func isAllowedTailnetIP(ip netip.Addr, status *ipnstate.Status, acceptRoutes bool) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	if ip.Is4() && cgnatPrefix.Contains(ip) {
		return true
	}
	if ip.Is6() && tailnetV6.Contains(ip) {
		return true
	}
	if !acceptRoutes || status == nil {
		return false
	}
	for _, peer := range status.Peer {
		if peer == nil {
			continue
		}
		for _, p := range prefixList(peer.PrimaryRoutes) {
			if p.Bits() > 0 && p.Contains(ip) {
				return true
			}
		}
	}
	return false
}

func formatIPList(ips []netip.Addr) string {
	parts := make([]string, len(ips))
	for i, ip := range ips {
		parts[i] = ip.Unmap().String()
	}
	return strings.Join(parts, ", ")
}

func (d *tailnetDialer) resolveAddr(ctx context.Context, network, addr string) (resolvedAddr string, resolved bool, err error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", false, err
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !d.tailnetOnly {
			return net.JoinHostPort(ip.String(), port), true, nil
		}
		norm := ip.Unmap()
		if isAllowedTailnetIP(norm, nil, false) {
			return net.JoinHostPort(norm.String(), port), true, nil
		}
		var status *ipnstate.Status
		if d.acceptRoutes {
			var statusErr error
			status, statusErr = d.fetchStatus(ctx)
			if statusErr != nil {
				slog.Debug("tailnet status lookup failed during -tailnet-only check", slog.String("host", host), slog.Any("error", statusErr))
				status = nil
			}
		}
		if !isAllowedTailnetIP(norm, status, d.acceptRoutes) {
			return "", false, fmt.Errorf("destination %q (%s) blocked by -tailnet-only", host, norm)
		}
		return net.JoinHostPort(norm.String(), port), true, nil
	}

	status, statusErr := d.fetchStatus(ctx)
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
		if d.tailnetOnly {
			return "", false, fmt.Errorf("destination %q blocked by -tailnet-only: tailnet DNS lookup failed: %w", host, err)
		}
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
		if d.tailnetOnly {
			return "", false, fmt.Errorf("destination %q blocked by -tailnet-only: DNS lookup returned no addresses", host)
		}
		if d.isStrictTailnetHost(status, host) {
			return "", false, fmt.Errorf("tailnet DNS lookup for %q returned no addresses", host)
		}
		return "", false, nil
	}

	chosen := ips[0]
	if d.tailnetOnly {
		var allowed netip.Addr
		for _, ip := range ips {
			if isAllowedTailnetIP(ip, status, d.acceptRoutes) {
				allowed = ip.Unmap()
				break
			}
		}
		if !allowed.IsValid() {
			return "", false, fmt.Errorf("destination %q (%s) blocked by -tailnet-only", host, formatIPList(ips))
		}
		chosen = allowed
	}

	resolvedAddr = net.JoinHostPort(chosen.String(), port)
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
	suffixes := append(append([]string{}, magicDNSSuffixes(status)...), d.getSuffixes()...)
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
		response, resolvers, err := d.queryDNS(ctx, dnsQueryName(host), queryType)
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

func startSOCKS5Proxy(ctx context.Context, listener net.Listener, dialer func(context.Context, string, string) (net.Conn, error), serveErr chan<- serveResult) {
	proxy := &socks5.Server{
		Logf: func(format string, args ...any) {
			slog.Debug(fmt.Sprintf(format, args...))
		},
		Dialer: func(_ context.Context, network, addr string) (net.Conn, error) {
			// tailscale.com/net/socks5.Server.handleTCP hardcodes a 5s dial
			// context derived from context.Background(), which cuts short our
			// dialer's timeout on cold starts, laptop wake, or slow DERP/WARP
			// handshakes. Pass the proxy's lifecycle ctx so tailnetDialer's
			// configured timeout governs and shutdown cancels in-flight dials.
			return dialer(ctx, network, addr)
		},
	}
	go func() {
		serveErr <- serveResult{name: "socks5", err: proxy.Serve(listener)}
	}()
}

func startHTTPProxy(ctx context.Context, listener net.Listener, dialer func(context.Context, string, string) (net.Conn, error), serveErr chan<- serveResult) {
	server := &http.Server{
		Handler:     httpProxyHandler(dialer),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		serveErr <- serveResult{name: "http", err: server.Serve(listener)}
	}()
}

type halfCloser interface {
	CloseWrite() error
}

type relayDstError struct{ error }

func (e relayDstError) Unwrap() error { return e.error }

type relayDstWriter struct{ io.Writer }

func (w relayDstWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err != nil {
		err = relayDstError{err}
	}
	return n, err
}

// pumpHalfClose copies src into dst until EOF or error, propagating EOF as a
// TCP half-close (CloseWrite) when dst supports it so the peer can finish
// draining its response before tearing down the tunnel.
func pumpHalfClose(dst net.Conn, src io.Reader) error {
	if _, err := io.Copy(relayDstWriter{dst}, src); err != nil {
		return err
	}
	if hc, ok := dst.(halfCloser); ok {
		if err := hc.CloseWrite(); err != nil {
			return relayDstError{err}
		}
	}
	return nil
}

// relayConn pumps bytes in both directions with half-close support. On clean
// EOF in one direction it waits for the reverse direction to finish draining;
// on a read error it closes both sockets to unblock the peer goroutine.
func relayConn(client, backend net.Conn, clientSrc io.Reader) {
	errc := make(chan error, 2)
	go func() { errc <- pumpHalfClose(client, backend) }()
	go func() { errc <- pumpHalfClose(backend, clientSrc) }()
	err := <-errc
	if err == nil {
		<-errc
		return
	}
	var dstErr relayDstError
	if !errors.As(err, &dstErr) {
		_ = backend.Close()
		_ = client.Close()
	}
	<-errc
}

func httpProxyHandler(dialer func(context.Context, string, string) (net.Conn, error)) http.Handler {
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {},
		Transport: &http.Transport{
			DialContext:     dialer,
			IdleConnTimeout: 90 * time.Second,
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

		relayConn(client, backend, clientSrc)
	})
}

// parseAdvertiseTags parses and validates a comma-separated list of Tailscale
// ACL tags. Each non-empty entry must start with "tag:" followed by a name.
func parseAdvertiseTags(s string) ([]string, error) {
	tags := splitCommaList(s)
	for _, tag := range tags {
		if !strings.HasPrefix(tag, "tag:") || len(tag) == len("tag:") || strings.ContainsAny(tag, " \t\r\n") {
			return nil, fmt.Errorf("invalid -advertise-tags entry %q: tags must have form tag:<name>", tag)
		}
	}
	return tags, nil
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
	type listenerEntry struct{ flag, addr string }
	listeners := []listenerEntry{
		{"-socks5", *flagSOCKS5Addr},
		{"-http", *flagHTTPAddr},
		{"-pac-addr", *flagPACAddr},
	}
	for _, f := range flagForwards {
		listeners = append(listeners, listenerEntry{"-L", f.local})
	}
	for _, l := range listeners {
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
