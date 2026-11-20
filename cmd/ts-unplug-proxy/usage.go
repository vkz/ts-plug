package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

const usageText = `ts-unplug-proxy - local SOCKS5/HTTP/PAC proxy, TCP forwarder, and tailnet CLI
running entirely in userspace via tsnet.

WHY THIS EXISTS
  On some machines you cannot run the normal Tailscale client: another tunnel
  (for example Cloudflare WARP / One) already owns the default route and DNS,
  and its address space collides with Tailscale's 100.64.0.0/10. tsnet avoids
  that entirely: it is a userspace WireGuard + netstack node, so tailnet
  100.x addresses never enter the kernel routing table. This proxy dials
  requested destinations through that tsnet node, so you can reach tailnet
  hosts from a browser, CLI, SSH, or TCP apps without installing tailscaled.

USAGE
  ts-unplug-proxy -dir <state-dir> (-socks5 <addr> | -http <addr> | -L <local=remote>) [-pac-addr <addr>] [options]
  ts-unplug-proxy <subcommand> [options] [args...]
  ts-unplug-proxy -dir <state-dir> -clear-exit-node
  ts-unplug-proxy -version

  Subcommands must be the first argument. Run 'ts-unplug-proxy <subcommand> -h'
  (or 'ts-unplug-proxy help <subcommand>') for detailed help and examples.

SUBCOMMANDS
  connect    OpenSSH ProxyCommand helper with -fdpass (ProxyUseFdpass yes)
  status     Show daemon readiness, hostname, listeners, key expiry, and conns
  peers      List tailnet peers, IPs, online state, and direct/DERP path
  ping       Ping a tailnet peer via the running daemon (shows RTT & DERP)
  login      Display/open the Tailscale auth URL and wait until Ready
  conns      List active tunneled connections (proto, target, age, bytes)
  env        Run a command with ALL_PROXY/HTTP_PROXY/HTTPS_PROXY/NO_PROXY set
  file       Taildrop file transfers: 'file cp', 'file get', 'file ls', 'file targets'
  cp         Shorthand alias for 'file cp' (<file...> <peer>:)
  doctor     Diagnose state dir, persisted prefs, listeners, PAC, and routes
  help       Show help for the main command or a subcommand

  In serve mode, at least one of -socks5, -http, or -L is required; a PAC alone
  has nothing to route to (and requires -socks5 or -http). Proxy clients choose
  the destination; serve mode does not take positional remote arguments.

LISTENERS
  -socks5 <addr>   SOCKS5 proxy. Clients must send the hostname (curl
                   --socks5-hostname, or socks5h://); tailnet names are then
                   resolved through Tailscale DNS.
  -http  <addr>    HTTP proxy (absolute-form requests and CONNECT tunnels).
                   CONNECT always carries the hostname, so this is the safest
                   choice for browsers.
  -L <local=remote> Static local TCP port forward (repeatable), e.g.
                    -L 127.0.0.1:7687=neo4j.wild-dojo.ts.net:7687.
  -control-sock <path>
                    Override path for the Unix control socket (default
                    <dir>/control.sock on Unix). Serves /localapi/, /status,
                    and /debug/connections with 0600 permissions.
  -pac-addr <addr> Serve /proxy.pac (and read-only GET /status) on this
                   address. Served before tsnet connects so browsers fail
                   closed; MagicDNS suffixes are discovered dynamically after
                   the netmap arrives (even on a fresh -dir without -pac-domain).

ROUTING AND RESOLUTION
  -dir <path>                    tsnet state directory (required for serve/doctor).
                                 Holds the node identity; keep it 0700.
  -hostname <name>               Tailnet hostname for the tsnet node (default
                                 "tsunplug-proxy").
  -advertise-tags <tag:a,...>    Comma-separated Tailscale ACL tags to advertise
                                 on this node (needed for tag-gated SSH ACLs).
  -dial-timeout <dur>            Timeout for dialing destinations through tsnet
                                 (default 30s; applies to SOCKS5, HTTP, and -L).
  -pac-domain <a,b>              Comma-separated tailnet DNS suffixes the PAC
                                 must never send DIRECT. Defaults to the
                                 MagicDNS name recorded in -dir or discovered
                                 live from the tailnet netmap.
  -accept-routes                 Accept advertised subnet routes and Tailscale
                                 Service VIPs (sets RouteAll). Required to reach
                                 Services or hosts behind subnet routers.
  -direct                        Force non-tailnet names through the OS resolver
                                 and dialer. Off by default, and rarely needed:
                                 without an exit node, non-tailnet traffic
                                 already egresses via the OS.
  -tailnet-only                  Block destinations outside tailnet IPs
                                 (100.64.0.0/10, fd7a:115c:a1e0::/48, and
                                 non-default peer routes when -accept-routes
                                 is set). Mutually exclusive with -exit-node
                                 and -direct.
  -disable-interface-binding     Disable Tailscale's network-namespace interface
                                 binding. Needed on macOS when another tunnel
                                 (WARP) owns interface/route selection.
  -exit-node                     Use a tailnet exit node for non-tailnet egress.
                                 Off by default: tailnet-host access only.
  -clear-exit-node               One-shot: clear a persisted exit-node
                                 preference from -dir, then exit. Stop the
                                 proxy first (it holds the state-dir lock).

EXIT NODES (READ THIS)
  Exit nodes are opt-in (-exit-node). tsnet.Up replaces the persisted prefs
  with defaults on every start, so this choice (like -accept-routes) is NOT
  carried across runs: pass the flags you want on every invocation, and a
  previously persisted selection is cleared on the next start. 'doctor'
  reports a persisted exit-node pref, and
  'ts-unplug-proxy -dir <state-dir> -clear-exit-node' scrubs the state file
  without starting tsnet (stop the proxy first; it holds
  <dir>/ts-unplug-proxy.lock).

PAC BEHAVIOUR (FAIL CLOSED)
  /proxy.pac returns the proxy list for:
    * any host equal to, or under, a configured/discovered suffix or .ts.net,
    * any literal address in 100.64.0.0/10 (Tailscale node IPs / Service VIPs),
    * any literal address in fd7a:115c:a1e0::/48 (Tailscale IPv6).
  Everything else returns DIRECT. Tailnet names are never sent DIRECT. The
  CGNAT check only runs for IP literals, so hostnames never trigger a PAC DNS
  lookup.

SECURITY
  Listeners (-socks5, -http, -pac-addr, -L) bind loopback by default and refuse
  non-loopback addresses unless -allow-non-loopback is passed. There is no proxy
  authentication; do not expose TCP listeners on a shared network. Privileged
  operations (peers, ping, login, /debug/connections, Taildrop) are served ONLY
  over the 0600 Unix control socket, never over -pac-addr.

LOGGING
  -v    Log proxy connections and dials.
  -vv   Also log tsnet internals.

ENVIRONMENT
  TS_AUTHKEY            Auth key for non-interactive initial node registration.
  TS_UNPLUG_PROXY_SOCK  Default control socket path for CLI subcommands.
  TS_UNPLUG_PROXY_DIR   Default state directory (uses <dir>/control.sock) for
                        CLI subcommands when -socket/-dir are not passed.

EXAMPLES
  # Full daemon for a machine behind Cloudflare WARP (with a static Bolt forward)
  ts-unplug-proxy -dir ~/.local/state/ts-unplug-proxy \
    -socks5 127.0.0.1:1080 -http 127.0.0.1:8080 \
    -pac-addr 127.0.0.1:8099 \
    -L 127.0.0.1:7687=neo4j.wild-dojo.ts.net:7687 \
    -disable-interface-binding -accept-routes

  # Inspect daemon, peers, latency, active connections, or log in
  ts-unplug-proxy status
  ts-unplug-proxy login -open
  ts-unplug-proxy peers
  ts-unplug-proxy ping gxa
  ts-unplug-proxy conns

  # Run a one-shot command through the running proxy
  ts-unplug-proxy env -- curl https://gxa.wild-dojo.ts.net/

  # Send and receive files via Taildrop
  ts-unplug-proxy cp report.pdf gxa:
  ts-unplug-proxy file ls
  ts-unplug-proxy file get ~/Downloads

  # OpenSSH (~/.ssh/config) using FD passing (zero lingering helper process)
  Match final host *.wild-dojo.ts.net,*.ts.net
    ProxyCommand ts-unplug-proxy connect -fdpass -status 127.0.0.1:8099 %h %p
    ProxyUseFdpass yes
    ServerAliveInterval 30

  # Diagnose state dir, listeners, PAC and routes
  ts-unplug-proxy doctor -dir ~/.local/state/ts-unplug-proxy \
    -http 127.0.0.1:8080 -pac-addr 127.0.0.1:8099
`

const doctorUsageText = `ts-unplug-proxy doctor - diagnose state dir, persisted prefs, listeners, PAC, and routes.

USAGE
  ts-unplug-proxy doctor [options]

OPTIONS
  -dir <path>                 tsnet state directory (default: $TS_UNPLUG_PROXY_DIR
                              or ${XDG_STATE_HOME:-~/.local/state}/ts-unplug-proxy)
  -control-sock <path>        override Unix control socket for live listener discovery
  -socks5 <addr>              SOCKS5 listener address to validate
  -http <addr>                HTTP proxy listener address to validate
  -L <local=remote>           static TCP forward to validate (repeatable)
  -pac-addr <addr>            PAC listener address to validate
  -pac-domain <a,b>           tailnet DNS suffixes to check in PAC decisions
  -hostname <name>            override persisted hostname for online route probe
  -advertise-tags <tag:a,...> override persisted tags for online route probe
  -accept-routes              report advertised/accepted peer routes
  -disable-interface-binding  disable macOS network namespace binding during probe
  -allow-non-loopback         allow non-loopback listeners (warns instead of failing)

Checks state prefs offline and only opens tsnet for the route check when no
running daemon holds the state-dir lock (preserving persisted Hostname and
AdvertiseTags unless overridden, and restoring the state file afterwards).
`

func printDoctorUsage() {
	_, _ = io.WriteString(os.Stderr, doctorUsageText)
}

func runHelp(args []string) int {
	if len(args) == 0 {
		printUsage()
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		printUsage()
		return 0
	case "connect":
		return runConnect([]string{"-h"})
	case "status":
		return runStatus([]string{"-h"})
	case "peers":
		return runPeers([]string{"-h"})
	case "ping":
		return runPing([]string{"-h"})
	case "login":
		return runLogin([]string{"-h"})
	case "conns":
		return runConns([]string{"-h"})
	case "env":
		return runEnv([]string{"-h"})
	case "file":
		if len(args) > 1 {
			return runFile([]string{args[1], "-h"})
		}
		return runFile([]string{"-h"})
	case "cp":
		return runCP([]string{"-h"})
	case "doctor":
		printDoctorUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "ts-unplug-proxy help: unknown subcommand %q\n\n", args[0])
		printUsage()
		return 2
	}
}

func printUsage() {
	_, _ = io.WriteString(os.Stderr, usageText)
	fmt.Fprintln(os.Stderr, "\nOPTIONS")
	fmt.Fprintln(os.Stderr)
	flagDefaults()
}

// flagDefaults prints the flag table (values and defaults).
func flagDefaults() {
	prev := flag.CommandLine.Output()
	flag.CommandLine.SetOutput(os.Stderr)
	flag.PrintDefaults()
	flag.CommandLine.SetOutput(prev)
}
