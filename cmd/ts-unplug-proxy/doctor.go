package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/netns"
	"tailscale.com/tsnet"
	"tailscale.com/types/views"
)

// tailnetV6 is Tailscale's IPv6 prefix (fd7a:115c:a1e0::/48).
var tailnetV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")

// doctor inspects the on-disk state and configuration and reports problems. It
// exits non-zero if it finds any FAIL. It is deliberately offline-first: it
// only opens tsnet for the route check when it can take the state-dir lock
// (i.e. no proxy is running).
type doctor struct {
	dir              string
	socksAddr        string
	httpAddr         string
	pacAddr          string
	pacDomains       []string
	allowNonLoopback bool

	problems int
	warnings int
}

func (d *doctor) fail(format string, args ...any) {
	d.problems++
	fmt.Printf("FAIL  "+format+"\n", args...)
}

func (d *doctor) warn(format string, args ...any) {
	d.warnings++
	fmt.Printf("WARN  "+format+"\n", args...)
}

func (d *doctor) ok(format string, args ...any)   { fmt.Printf("ok    "+format+"\n", args...) }
func (d *doctor) info(format string, args ...any) { fmt.Printf("      "+format+"\n", args...) }
func (d *doctor) section(name string)             { fmt.Printf("\n%s:\n", name) }

// runDoctor executes all checks and returns a process exit code.
func runDoctor() int {
	d := &doctor{
		dir:              *flagDir,
		socksAddr:        *flagSOCKS5Addr,
		httpAddr:         *flagHTTPAddr,
		pacAddr:          *flagPACAddr,
		pacDomains:       splitCommaList(*flagPACDomains),
		allowNonLoopback: *flagAllowNonLoopback,
	}
	fmt.Println("ts-unplug-proxy doctor")
	d.section("state")
	d.checkState()
	d.section("listeners")
	d.checkListeners()
	d.section("pac")
	d.checkPAC()
	d.section("routes")
	d.checkRoutes()

	fmt.Println()
	if d.problems > 0 {
		fmt.Printf("%d problem(s), %d warning(s)\n", d.problems, d.warnings)
		return 1
	}
	fmt.Printf("no problems, %d warning(s)\n", d.warnings)
	return 0
}

func (d *doctor) checkState() {
	if d.dir == "" {
		d.fail("-dir is required")
		return
	}
	d.info("state dir: %s", d.dir)
	sf, err := loadStateFile(d.dir)
	if err != nil {
		d.fail("cannot read state: %v", err)
		return
	}
	if n := sf.magicDNSName(); n != "" {
		d.info("MagicDNS suffix: %s", n)
	} else {
		d.warn("could not determine MagicDNS suffix from state (set -pac-domain explicitly)")
	}
	p, err := sf.prefs()
	if err != nil {
		d.fail("cannot read prefs: %v", err)
		return
	}
	switch {
	case p.AutoExitNode != "":
		d.fail("persisted AutoExitNode=%q: this node will route non-tailnet traffic through an exit node", p.AutoExitNode)
	case p.ExitNodeID != "" || p.ExitNodeIP != "":
		d.fail("persisted exit node (ID=%q IP=%q)", p.ExitNodeID, p.ExitNodeIP)
	default:
		d.ok("no persisted exit-node preference")
	}
	if p.AutoExitNode != "" || p.ExitNodeID != "" || p.ExitNodeIP != "" {
		d.info("clear it with the proxy stopped: ts-unplug-proxy -dir %q -clear-exit-node", d.dir)
	}
	d.info("WantRunning=%v CorpDNS=%v RouteAll=%v", p.WantRunning, p.CorpDNS, p.RouteAll)
	if len(p.AdvertiseRoutes) > 0 {
		d.info("advertised routes (prefs): %s", strings.Join(p.AdvertiseRoutes, ", "))
	} else {
		d.info("advertised routes (prefs): none")
	}
	if p.RouteAll {
		d.info("RouteAll=true: accepts every advertised subnet route and Service VIP")
	}
}

func (d *doctor) checkListeners() {
	for _, l := range []struct{ flag, addr string }{
		{"-socks5", d.socksAddr},
		{"-http", d.httpAddr},
		{"-pac-addr", d.pacAddr},
	} {
		if l.addr == "" {
			continue
		}
		switch {
		case isLoopbackListenAddr(l.addr):
			d.ok("%s %s is loopback-only", l.flag, l.addr)
		case d.allowNonLoopback:
			d.warn("%s %s is NOT loopback (allowed by -allow-non-loopback)", l.flag, l.addr)
		default:
			d.fail("%s %s is NOT loopback and -allow-non-loopback is not set", l.flag, l.addr)
		}
	}
	if d.socksAddr == "" && d.httpAddr == "" {
		d.fail("no -socks5 or -http listener configured (a PAC alone has nothing to route to)")
	}
}

func (d *doctor) checkPAC() {
	if d.pacAddr == "" {
		d.info("disabled (set -pac-addr to serve /proxy.pac)")
		return
	}
	domains := d.pacDomains
	if len(domains) == 0 && d.dir != "" {
		if sf, err := loadStateFile(d.dir); err == nil {
			if n := sf.magicDNSName(); n != "" {
				domains = []string{n}
			}
		}
	}
	if len(domains) == 0 {
		d.fail("-pac-addr is set but no -pac-domain and no MagicDNS suffix in state")
		return
	}
	cfg := pacConfig{suffixes: domains, proxies: pacProxies(d.socksAddr, d.httpAddr)}
	if cfg.proxyList() == "" {
		d.fail("-pac-addr is set but neither -socks5 nor -http is configured")
		return
	}
	d.info("proxies:  %s", cfg.proxyList())
	d.info("suffixes: %s", strings.Join(cfg.effectiveSuffixes(), ", "))
	// Fail-closed must hold for tailnet names and CGNAT literals; only
	// unmatched hosts may go DIRECT.
	samples := []struct {
		host string
		want string
	}{
		{domains[0], "proxy"},
		{"gxa." + domains[0], "proxy"},
		{"100.64.1.1", "proxy"},
		{"100.127.255.254", "proxy"},
		{"example.com", "direct"},
		{"100.63.255.255", "direct"},
		{"100.128.0.1", "direct"},
	}
	for _, s := range samples {
		got := "direct"
		if cfg.match(s.host) != "DIRECT" {
			got = "proxy"
		}
		if got != s.want {
			d.fail("PAC decision for %s is %s, want %s", s.host, got, s.want)
		} else {
			d.ok("PAC %s -> %s", s.host, got)
		}
	}
}

func (d *doctor) checkRoutes() {
	if d.dir == "" {
		d.warn("skipping route check: no -dir")
		return
	}
	// The state file has no cross-process lock, so a second tsnet on the same
	// node key would corrupt it. Refuse while a proxy holds the dir lock.
	lock, err := lockDir(d.dir)
	if err != nil {
		d.warn("state dir is in use by a running proxy; skipping online route check (%v)", err)
		return
	}
	defer lock.Close()
	// tsnet.Up replaces the persisted prefs with defaults (Hostname, WantRunning,
	// ControlURL, ...), so probing with a second tsnet.Server would mutate the
	// state dir. Snapshot the prefs bytes and restore them when done.
	snapKey, snapRaw, snapOK := d.snapshotPrefs()
	func() {
		if *flagDisableInterfaceBinding {
			netns.SetEnabled(false)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ts := &tsnet.Server{
			Hostname: tsnetHostname,
			Dir:      d.dir,
			Logf: func(format string, args ...any) {
				slog.Debug(fmt.Sprintf(format, args...))
			},
		}
		defer ts.Close()
		if _, err := ts.Up(ctx); err != nil {
			d.warn("tsnet did not come up within 30s (%v); route check skipped", err)
			return
		}
		lc, err := ts.LocalClient()
		if err != nil {
			d.warn("no local client: %v", err)
			return
		}
		status := waitForNetmap(ctx, lc, 15*time.Second)
		if status == nil {
			d.warn("status unavailable; route check skipped")
			return
		}
		if status.Self != nil {
			selfRoutes := prefixList(status.Self.PrimaryRoutes)
			if len(selfRoutes) == 0 {
				d.ok("advertises no routes")
			} else {
				d.info("advertised: %s", prefixesString(selfRoutes))
			}
		}
		// Route acceptance is decided by the flags the caller will run the
		// proxy with, not by the persisted prefs (which tsnet.Up resets).
		if !*flagAcceptRoutes {
			d.ok("-accept-routes not set: not accepting peer subnet routes")
			return
		}
		var accepted []netip.Prefix
		for _, peer := range status.Peer {
			accepted = append(accepted, prefixList(peer.PrimaryRoutes)...)
		}
		if len(accepted) == 0 {
			d.ok("no subnet routes advertised by peers")
			return
		}
		var nonTailnet []netip.Prefix
		for _, p := range accepted {
			if !isTailnetPrefix(p) {
				nonTailnet = append(nonTailnet, p)
			}
		}
		if len(nonTailnet) > 0 {
			d.warn("accepting %d non-tailnet route(s): %s; the proxy will send these CIDRs through the tailnet, which can shadow WARP/corporate networks", len(nonTailnet), prefixesString(nonTailnet))
		} else {
			d.ok("all %d accepted route(s) are inside tailnet space", len(accepted))
		}
	}()
	d.restorePrefs(snapKey, snapRaw, snapOK)
}

func (d *doctor) snapshotPrefs() (key string, raw []byte, ok bool) {
	sf, err := loadStateFile(d.dir)
	if err != nil {
		return "", nil, false
	}
	key, raw, err = sf.rawPrefs()
	if err != nil {
		return "", nil, false
	}
	return key, append([]byte(nil), raw...), true
}

func (d *doctor) restorePrefs(key string, raw []byte, ok bool) {
	if !ok {
		return
	}
	sf, err := loadStateFile(d.dir)
	if err != nil {
		d.warn("could not reload state to restore prefs: %v", err)
		return
	}
	if err := sf.setRawPrefs(key, raw); err != nil {
		d.warn("could not restore prefs after route probe: %v", err)
		return
	}
	d.info("restored tsnet-managed prefs after route probe")
}

// waitForNetmap polls until the network map has been applied (self is in it),
// so peers and routes are populated. Reading Status immediately after Up races
// the netmap. Bounded by timeout.
func waitForNetmap(ctx context.Context, lc *local.Client, timeout time.Duration) *ipnstate.Status {
	deadline := time.Now().Add(timeout)
	var last *ipnstate.Status
	for {
		st, err := lc.Status(ctx)
		if err == nil {
			last = st
			if st.Self != nil && st.Self.InNetworkMap {
				return st
			}
		}
		if time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func isTailnetPrefix(p netip.Prefix) bool {
	addr := p.Masked().Addr()
	if addr.Is4() {
		return cgnatPrefix.Contains(addr)
	}
	return tailnetV6.Contains(addr)
}

func prefixesString(ps []netip.Prefix) string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	return strings.Join(out, ", ")
}

// prefixList converts a views.Slice into a plain slice, tolerating nil.
func prefixList(s *views.Slice[netip.Prefix]) []netip.Prefix {
	if s == nil || s.IsNil() {
		return nil
	}
	return s.AsSlice()
}
