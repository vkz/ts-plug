package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

const usageText = `ts-unplug-proxy - local SOCKS5/HTTP/PAC proxy into a Tailscale tailnet,
running entirely in userspace via tsnet.

WHY THIS EXISTS
  On some machines you cannot run the normal Tailscale client: another tunnel
  (for example Cloudflare WARP / One) already owns the default route and DNS,
  and its address space collides with Tailscale's 100.64.0.0/10. tsnet avoids
  that entirely: it is a userspace WireGuard + netstack node, so tailnet
  100.x addresses never enter the kernel routing table. This proxy dials
  requested destinations through that tsnet node, so you can reach tailnet
  hosts from a browser, CLI, or SSH without installing tailscaled.

USAGE
  ts-unplug-proxy -dir <state-dir> (-socks5 <addr> | -http <addr>) [-pac-addr <addr>] [options]
  ts-unplug-proxy connect [-socks5 <addr> | -http <addr>] [-fdpass] [-status <addr>] <host> <port>
  ts-unplug-proxy doctor -dir <state-dir> [-socks5 <addr>] [-http <addr>] [-pac-addr <addr>] [-hostname <name>] [-advertise-tags <tags>]
  ts-unplug-proxy -dir <state-dir> -clear-exit-node
  ts-unplug-proxy -version

  Subcommands ('connect', 'doctor') must be the first argument. At least one of
  -socks5 or -http is required to serve; a PAC alone has nothing to route to.
  Proxy clients choose the destination; serve mode does not take a remote
  address argument.

  'connect' is a lightweight client helper for OpenSSH ProxyCommand (run
  'ts-unplug-proxy connect -h' for its flags). With -fdpass and OpenSSH
  "ProxyUseFdpass yes", it completes the proxy handshake, hands the connected
  TCP socket FD directly to ssh, and exits immediately.

LISTENERS
  -socks5 <addr>   SOCKS5 proxy. Clients must send the hostname (curl
                   --socks5-hostname, or socks5h://); tailnet names are then
                   resolved through Tailscale DNS.
  -http  <addr>    HTTP proxy (absolute-form requests and CONNECT tunnels).
                   CONNECT always carries the hostname, so this is the safest
                   choice for browsers.
  -pac-addr <addr> Serve /proxy.pac (and GET /status, a JSON health document)
                   on this address. The PAC sends tailnet hosts to the proxy
                   and everything else DIRECT. It is served before tsnet
                   connects, so a browser never falls back to DIRECT because
                   the PAC was momentarily unavailable.

ROUTING AND RESOLUTION
  -dir <path>                    tsnet state directory (required). Holds the
                                 node identity; keep it 0700.
  -hostname <name>               Tailnet hostname for the tsnet node (default
                                 "tsunplug-proxy").
  -advertise-tags <tag:a,...>    Comma-separated Tailscale ACL tags to advertise
                                 on this node (needed for tag-gated SSH ACLs).
  -dial-timeout <dur>            Timeout for dialing destinations through tsnet
                                 (default 30s; applies to both SOCKS5 and HTTP).
  -pac-domain <a,b>              Comma-separated tailnet DNS suffixes the PAC
                                 must never send DIRECT. Defaults to the
                                 MagicDNS name recorded in -dir.
  -accept-routes                 Accept advertised subnet routes and Tailscale
                                 Service VIPs (sets RouteAll). Required to reach
                                 Services or hosts behind subnet routers.
  -direct                        Force non-tailnet names through the OS resolver
                                 and dialer. Off by default, and rarely needed:
                                 without an exit node, non-tailnet traffic
                                 already egresses via the OS.
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
    * any host equal to, or under, a configured suffix or .ts.net, and
    * any literal address in 100.64.0.0/10 (Tailscale node IPs / Service VIPs).
  Everything else returns DIRECT. Tailnet names are never sent DIRECT: a
  Funnel name under .ts.net can resolve publicly, so falling through to DIRECT
  would leak tailnet-named traffic onto the internet. The CGNAT check only runs
  for IP literals, so hostnames never trigger a PAC DNS lookup.

SECURITY
  Listeners bind loopback by default and refuse non-loopback addresses unless
  -allow-non-loopback is passed. There is no proxy authentication; do not
  expose these listeners on a shared network.

LOGGING
  -v    Log proxy connections and dials.
  -vv   Also log tsnet internals.

ENVIRONMENT
  TS_AUTHKEY   If set and the node is not yet registered, tsnet uses it to
               authenticate non-interactively (useful for a boot service).

EXAMPLES
  # SOCKS5, then curl a tailnet host through it
  ts-unplug-proxy -dir ~/.local/state/ts-unplug-proxy -socks5 127.0.0.1:1080
  curl --socks5-hostname 127.0.0.1:1080 https://gxa.wild-dojo.ts.net/

  # HTTP proxy + PAC, the browser setup
  ts-unplug-proxy -dir ~/.local/state/ts-unplug-proxy \
    -http 127.0.0.1:8080 -pac-addr 127.0.0.1:8099 -pac-domain wild-dojo.ts.net
  # point Chrome / macOS at http://127.0.0.1:8099/proxy.pac

  # Full setup for a machine behind WARP
  ts-unplug-proxy -dir ~/.local/state/ts-unplug-proxy \
    -socks5 127.0.0.1:1080 -http 127.0.0.1:8080 \
    -pac-addr 127.0.0.1:8099 -pac-domain wild-dojo.ts.net \
    -disable-interface-binding -accept-routes

  # OpenSSH (~/.ssh/config) using FD passing (zero lingering helper process)
  Match final host *.wild-dojo.ts.net,*.ts.net
    ProxyCommand ts-unplug-proxy connect -fdpass -status 127.0.0.1:8099 %h %p
    ProxyUseFdpass yes
    ServerAliveInterval 30

  # Diagnose the state dir, listeners, PAC and routes
  ts-unplug-proxy doctor -dir ~/.local/state/ts-unplug-proxy \
    -http 127.0.0.1:8080 -pac-addr 127.0.0.1:8099 -pac-domain wild-dojo.ts.net
`

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
