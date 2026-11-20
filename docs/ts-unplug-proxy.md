# ts-unplug-proxy Guide

**ts-unplug-proxy** runs local SOCKS5/HTTP/PAC proxies, static TCP port forwards,
and a full set of tailnet CLI tools (`status`, `peers`, `ping`, `login`,
`conns`, `env`, `connect`, `file`/`cp`, `doctor`) through a single userspace
`tsnet.Server` — no kernel `tailscaled` required.

## Overview

ts-unplug-proxy allows you to:
- Use one userspace `tsnet` node for browsers (PAC/HTTP), SSH (`connect -fdpass`), CLI tools (`env`), and TCP apps (`-L` forwards)
- Inspect and operate your tailnet (`status`, `peers`, `ping`, `login`, `conns`, `file`/`cp` Taildrop) even on machines where `tailscaled` and the `tailscale` CLI cannot run
- Resolve tailnet hostnames through Tailscale status/DNS instead of local DNS
- Stay completely out of the host kernel routing table (coexisting cleanly with Cloudflare WARP/One)

## Subcommand Reference

Run `ts-unplug-proxy <subcommand> -h` (or `ts-unplug-proxy help <subcommand>`) for complete flags and examples.

| Subcommand | Purpose |
|------------|---------|
| *(default)* | Run the daemon (`-dir` + `-socks5`, `-http`, and/or `-L`) |
| `connect` | OpenSSH `ProxyCommand` helper supporting `ProxyUseFdpass yes` (`-fdpass`) |
| `status` | Show daemon state, hostname, listeners, `key_expiry` warning, and active connection count |
| `peers` | List tailnet peers, IPs, online status, and direct vs DERP relay path |
| `ping` | Ping a peer via the running daemon to check latency and direct vs DERP |
| `login` | Display (and optionally `-open`) the Tailscale auth URL and wait until Ready |
| `conns` | List active tunneled connections (protocol, target, age, bytes in/out) |
| `env` | Run a one-shot command with `ALL_PROXY`/`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` set (or `-print` exports) |
| `file` | Taildrop transfers: `file cp`, `file get`, `file ls`, `file targets` |
| `cp` | Shorthand alias for `file cp` (`ts-unplug-proxy cp <file...> <peer>:`) |
| `doctor` | Diagnose state dir, persisted exit-node prefs, listeners, PAC, and routes |
| `help` | Print help for the root command or any subcommand |

## Installation

Build from source:
```sh
make ts-unplug-proxy
```

Install to `$GOPATH/bin`:
```sh
make install
```

## Basic Usage

```sh
ts-unplug-proxy -dir <state-dir> (-socks5 <listen:port> | -http <listen:port>) [options]
```

Start a SOCKS5 proxy:
```sh
ts-unplug-proxy -dir ./state -socks5 127.0.0.1:1080
curl --socks5-hostname 127.0.0.1:1080 http://myserver.tailnet.ts.net
```

For SOCKS5, use `socks5h://` or `--socks5-hostname` so the client sends the hostname to the proxy. Tailnet names are resolved from Tailscale status/DNS; public names fall back to normal tsnet egress.

Start an HTTP proxy:
```sh
ts-unplug-proxy -dir ./state -http 127.0.0.1:8080
curl -x http://127.0.0.1:8080 http://myserver.tailnet.ts.net
```

Start both on different ports:
```sh
ts-unplug-proxy -dir ./state -socks5 127.0.0.1:1080 -http 127.0.0.1:8080
```

Start both on the same port:
```sh
ts-unplug-proxy -dir ./state -socks5 127.0.0.1:1080 -http 127.0.0.1:1080
```

## Configuration Flags

### Required

- `-dir` - Directory for tsnet server state
  - Stores Tailscale authentication and connection state
  - Example: `-dir ./state`, `-dir /var/lib/tsunplug-proxy`

- At least one listener flag:
  - `-socks5 <listen:port>` - Enable a SOCKS5 proxy listener
  - `-http <listen:port>` - Enable an HTTP proxy listener
  - `-L <local=remote>` - Static local TCP port forward (repeatable, e.g. `-L 127.0.0.1:7687=neo4j.wild-dojo.ts.net:7687`). Can be used alongside proxies or standalone (`-L`-only mode).

### Optional

- `-hostname <name>` - Tailnet hostname for the `tsnet.Server` node (default: `tsunplug-proxy`). Useful when running `ts-unplug-proxy` on multiple machines.

- `-advertise-tags <tag:a,tag:b>` - Comma-separated Tailscale ACL tags to advertise on this node (for example `tag:laptop-proxy`). Required if your tailnet ACLs or Tailscale SSH rules grant access to tag-based sources rather than personal user identities.

- `-dial-timeout <duration>` - Timeout for dialing destinations through `tsnet` (default: `30s`). Applies to SOCKS5, HTTP CONNECT, and `-L` forwards.

- `-control-sock <path>` - Override the Unix control socket path (default `<dir>/control.sock` on Unix). Useful if `-dir` is deep and approaches macOS's 104-byte Unix socket path limit.

- `-tailnet-only` - Enforce destination IP allowlisting at the proxy: allow only Tailscale CGNAT (`100.64.0.0/10`), Tailscale IPv6 (`fd7a:115c:a1e0::/48`), and (when `-accept-routes` is set) non-default peer `PrimaryRoutes` prefixes. Rejects public hostnames/IPs and unresolved fallback dials. Mutually exclusive with `-exit-node` and `-direct`.

- `-v` - Log proxy connections and proxy dials
  ```sh
  ts-unplug-proxy -dir ./state -v -socks5 127.0.0.1:1080
  ```

- `-vv` - Log proxy connections, proxy dials, and `tsnet.Server` debug info
  ```sh
  ts-unplug-proxy -dir ./state -vv -socks5 127.0.0.1:1080
  ```

- `-accept-routes` - Accept subnet and Tailscale Service routes advertised by other nodes

- `-disable-interface-binding` - On macOS, disable Tailscale's network namespace integration, including physical-interface binding. Use this when another network tunnel controls the system's interface and route selection.

- `-pac-addr <addr>` - Serve `/proxy.pac` and read-only `/status` on this address (loopback only unless `-allow-non-loopback`). Requires `-socks5` or `-http`. Works even on a fresh `-dir` without `-pac-domain`: MagicDNS suffixes are discovered dynamically once the netmap arrives.

- `-pac-domain <a,b>` - Comma-separated tailnet DNS suffixes the PAC must never send `DIRECT`. Optional: defaults to the MagicDNS name recorded in `-dir` or discovered live from the netmap.

- `-exit-node` - Use a tailnet exit node for non-tailnet egress. Off by default. Mutually exclusive with `-tailnet-only`. See [Exit nodes and persisted prefs](#exit-nodes-and-persisted-prefs).

- `-clear-exit-node` - One-shot: clear a persisted exit-node preference from `-dir`, then exit. Stop the proxy first.

- `-direct` - Force non-tailnet names through the OS resolver and dialer. Off by default and rarely needed: without an exit node, non-tailnet traffic already egresses via the OS. Does not apply to IP literals. Mutually exclusive with `-tailnet-only`.

- `-allow-non-loopback` - Allow `-socks5`/`-http`/`-pac-addr`/`-L` to bind non-loopback addresses. Off by default. When `-pac-addr` is non-loopback, `/status` over TCP omits `auth_url`.

- `-version` - Print version and build information, then exit.

## Browser setup with a PAC file

Serve the PAC from the same process and point the browser (or the system proxy) at it:

```sh
ts-unplug-proxy -dir ./state \
  -socks5 127.0.0.1:1080 -http 127.0.0.1:8080 \
  -pac-addr 127.0.0.1:8099 -pac-domain wild-dojo.ts.net
```

Then set the browser's automatic proxy configuration URL to
`http://127.0.0.1:8099/proxy.pac` (on macOS,
`networksetup -setautoproxyurl "Wi-Fi" http://127.0.0.1:8099/proxy.pac`).

The PAC is served as soon as the process binds its listeners, before tsnet has
connected, so a browser never falls back to `DIRECT` because the PAC was
momentarily unavailable. While tsnet connects, tailnet requests queue rather
than leaking. Even on a brand-new `-dir` with no `-pac-domain` flag, the initial
PAC fails closed for `.ts.net`, `100.64.0.0/10`, and `fd7a:115c:a1e0::/48`; once
the netmap arrives, the daemon atomically updates the served PAC, `/status`, and
the dialer with the discovered MagicDNS suffix (merging any explicit
`-pac-domain` values). Note that browsers may cache a PAC briefly until they
refresh it, so passing `-pac-domain` is still recommended if you use a custom
non-`.ts.net` suffix and want it active on the very first request.

`GET /status` on the PAC address returns a read-only JSON health document
(`ready`, `serving`, `hostname`, `backend_state`, `auth_url`, `key_expiry`,
`active_connections`, `socks5_addr`, `http_addr`, `control_sock`, `forwards`,
`tailnet_only`, `route_all`, `exit_node`, `suffixes`, `proxies`, `version`) and
returns HTTP `200` only when the backend is `Running` and the proxy listeners
are accepting connections. When `-pac-addr` is bound to a non-loopback address,
`auth_url` is omitted from TCP `/status` (it remains available on the `0600` Unix
control socket).

The PAC fails closed: any host under a configured suffix or `.ts.net`, and any
literal address in `100.64.0.0/10` (Tailscale node IPs and Service VIPs), is
sent to the proxy and never to `DIRECT`. Only unmatched hosts fall through to
`DIRECT`. `isInNet` runs only for IP literals, so hostnames never trigger a PAC
DNS lookup.

> `ts.net` is on the browser HSTS preload list, so plain `http://host.ts.net`
> is upgraded to HTTPS by Chrome/Safari/Firefox. Terminate TLS on the tailnet
> host (for example with `tailscale serve`) if you need browser access.

## Exit nodes and persisted prefs

Exit nodes are opt-in (`-exit-node`). `tsnet.Up` **replaces the persisted prefs
with defaults on every start** (`ipn.NewPrefs()` plus
hostname/WantRunning/ControlURL), so the choice is not carried across runs: pass
`-accept-routes` and the exit-node preference on every invocation. A previously
persisted exit-node selection is cleared on the next start, even without
`-exit-node`.

`-clear-exit-node` scrubs a persisted selection from the state file *without*
starting tsnet (for example before handing the directory to another tool), and
`doctor` reports one when present:

```sh
ts-unplug-proxy -dir ./state -clear-exit-node
```

## Diagnosing with `doctor`

`doctor` inspects the state dir, listeners, PAC and routes, and exits non-zero
on problems. It reads prefs offline and only opens tsnet for the route check
when it can take the state-dir lock (i.e. the proxy is not running). During the
online route probe it reuses the persisted `Hostname` and `AdvertiseTags` from
`-dir` unless overridden with `-hostname` / `-advertise-tags`, and restores the
state file afterwards.

```sh
ts-unplug-proxy doctor -dir ./state \
  -http 127.0.0.1:8080 -pac-addr 127.0.0.1:8099 -pac-domain wild-dojo.ts.net
```

It reports:

- the MagicDNS suffix and the effective prefs (warns/fails on a persisted exit node),
- whether every listener is loopback-only,
- the exact PAC decisions for sample hosts (tailnet, CGNAT, and public),
- advertised routes, and accepted routes with a warning when `RouteAll` pulls
  in non-tailnet (corporate) CIDRs.

## Control Socket & Tailnet CLI (`status`, `peers`, `ping`, `login`, `conns`)

On Unix (macOS/Linux), the running daemon serves a `0600` control socket at
`<dir>/control.sock` (override with `-control-sock`). It starts immediately
after taking the state-dir lock — before `ts.Up` finishes — so `status` and
`login` work while the node is in `NeedsLogin`.

The control socket reverse-proxies `/localapi/` to the in-process `tsnet`
LocalAPI and serves `/status` and `/debug/connections`. Privileged operations
(`peers`, `ping`, `login`, connection targets, and Taildrop) are served **only**
on this `0600` Unix socket, never on the TCP `-pac-addr` listener, preventing
browser CSRF or DNS rebinding.

> Because `/localapi/` is exposed natively on `<dir>/control.sock`, even the
> stock `tailscale` CLI works against the running daemon:
> `tailscale --socket ~/.local/state/ts-unplug-proxy/control.sock status`

### Control Socket Discovery

All CLI subcommands (`status`, `peers`, `ping`, `login`, `conns`, `env`, `file`,
`cp`) locate the daemon socket in this order:

1. `-socket <path>`
2. `-dir <dir>` (uses `<dir>/control.sock`)
3. `$TS_UNPLUG_PROXY_SOCK`
4. `$TS_UNPLUG_PROXY_DIR/control.sock`
5. `${XDG_STATE_HOME:-~/.local/state}/ts-unplug-proxy/control.sock`

Setting `export TS_UNPLUG_PROXY_DIR="$HOME/.local/state/ts-unplug-proxy"` (or your custom `-dir` path) in your
shell profile enables zero-flag CLI commands.

### Inspecting Status, Peers, Latency, and Connections

```sh
# Health summary + key_expiry warning (exits 0 when Running & serving, 1 otherwise)
ts-unplug-proxy status
ts-unplug-proxy status -json

# Log in interactively (prints auth URL, optionally opens browser, waits until Ready)
ts-unplug-proxy login -open

# List peers, IPs, online state, and direct vs DERP relay path
ts-unplug-proxy peers
ts-unplug-proxy peers -online
ts-unplug-proxy peers -json

# Ping a peer through the running daemon (shows RTT and direct vs DERP)
ts-unplug-proxy ping gxa
ts-unplug-proxy ping -c 5 -type disco 100.64.0.10

# Inspect live tunneled connections (SOCKS5, HTTP, and -L forwards)
ts-unplug-proxy conns
ts-unplug-proxy conns -json
```

## Static Local TCP Port Forwards (`-L`)

For proxy-unaware TCP tools (database drivers such as Neo4j Bolt or `psql`,
Redis, DB GUIs, local scripts), pass repeatable `-L <local-addr>=<remote:port>`
flags to reuse the same `tsnet` node without running a separate `ts-unplug`
process:

```sh
ts-unplug-proxy -dir ~/.local/state/ts-unplug-proxy \
  -socks5 127.0.0.1:1080 \
  -L 127.0.0.1:7687=neo4j.wild-dojo.ts.net:7687 \
  -L 127.0.0.1:5432=pg.wild-dojo.ts.net:5432 \
  -disable-interface-binding
```

`-L` also works standalone (without `-socks5` or `-http`). Every local bind
address is checked by the loopback safety rail (non-loopback requires
`-allow-non-loopback`), relays with full-duplex TCP half-close, and appears in
`ts-unplug-proxy conns`.

## One-Shot Proxy Wrapper (`env`)

Run a single command with `ALL_PROXY`, `HTTP_PROXY`, `HTTPS_PROXY`, and
`NO_PROXY` configured without polluting your global shell environment (in
SOCKS5-only mode, inherited `HTTP_PROXY`/`HTTPS_PROXY` are unset so they cannot
override `ALL_PROXY`):

```sh
# Auto-discovers listeners from the running daemon's control socket
ts-unplug-proxy env -- curl https://gxa.wild-dojo.ts.net/
ts-unplug-proxy env -- git fetch origin

# Print POSIX export statements for a subshell
eval "$(ts-unplug-proxy env -print)"

# Explicit listeners or extra NO_PROXY entries
ts-unplug-proxy env -socks5 127.0.0.1:1080 -http 127.0.0.1:8080 -no-proxy ".corp.local" -- npm test
```

> Note: if the daemon was started with `-tailnet-only`, commands run under `env`
> will have non-tailnet requests rejected by the proxy.

## Tailnet-Only Guard (`-tailnet-only`)

Pass `-tailnet-only` to enforce destination IP allowlisting at the proxy itself
(defense-in-depth if an app is pointed globally at `127.0.0.1:1080`):

- Allows only Tailscale IPv4 (`100.64.0.0/10`), Tailscale IPv6
  (`fd7a:115c:a1e0::/48`), and — when `-accept-routes` is also enabled —
  non-default (`Bits() > 0`) peer `PrimaryRoutes` prefixes.
- Enforces the check on the **final normalized IP** (not hostname alone) and
  disables unresolved fallback dials, so public hostnames and public IP literals
  are always rejected.
- Mutually exclusive with `-exit-node` and `-direct`.

## Taildrop File Transfers (`file` / `cp`)

`ts-unplug-proxy` registers Tailscale's Taildrop extension on its `tsnet` node,
so you can send and receive files directly from a machine behind WARP:

```sh
# List eligible Taildrop targets
ts-unplug-proxy file targets

# Send one or more files (destination must end with ':')
ts-unplug-proxy cp report.pdf gxa:
ts-unplug-proxy file cp a.txt b.txt gxa.wild-dojo.ts.net:
echo "hello" | ts-unplug-proxy cp -name note.txt - gxa:

# List waiting incoming files
ts-unplug-proxy file ls

# Receive waiting files safely into ~/Downloads (default conflict policy: skip)
ts-unplug-proxy file get ~/Downloads
ts-unplug-proxy file get -conflict rename ~/Downloads
ts-unplug-proxy file get -conflict overwrite -keep ~/Downloads
```

Safety & caveats:
- `file get` strictly rejects unsafe filenames (path separators, `..`, NUL
  bytes), streams to a temp file in the destination directory with `fsync` +
  atomic placement, and deletes the staged file from the daemon **only** after
  durable write succeeds (unless `-keep` is passed).
- Taildrop transfers require untagged user-owned nodes (or explicit Tailscale
  ACL file-sharing capabilities when using `-advertise-tags`).

## SSH over ts-unplug-proxy

`ts-unplug-proxy` relays arbitrary TCP connections (both SOCKS5 `CONNECT` and
HTTP `CONNECT` support full-duplex half-close with no idle timeout), so SSH,
`scp`, `sftp`, `rsync -e ssh`, `git` over SSH, port forwards (`-L`/`-R`), agent
forwarding, Emacs TRAMP, and VS Code Remote-SSH all work without installing
`tailscaled`.

Because SSH ignores system proxy settings and PAC files, configure OpenSSH's
`ProxyCommand` for your tailnet hosts.

### Recommended setup (`ts-unplug-proxy connect -fdpass`)

The built-in `connect` subcommand dials the local proxy using remote hostname
resolution (SOCKS5h / HTTP CONNECT), supports OpenSSH `ProxyUseFdpass yes`
(passing the connected TCP socket FD directly to `ssh` via `SCM_RIGHTS` and
exiting immediately so no helper process lingers), works identically on macOS
and Linux with no extra tools installed, and prints actionable errors to stderr.

In `~/.ssh/config` (or an included file such as `~/.ssh/config.d/tsnet-proxy.conf`):

```sshconfig
Match final host *.wild-dojo.ts.net,*.ts.net
  ProxyCommand ts-unplug-proxy connect -fdpass -status 127.0.0.1:8099 %h %p
  ProxyUseFdpass yes
  ServerAliveInterval 30
  ServerAliveCountMax 3
```

If GUI apps (such as VS Code Remote-SSH on macOS) launch `ssh` without your
shell's `PATH`, use `~` (or `$HOME` or an absolute path) — `ProxyCommand` runs
through your shell, so `~` expands reliably (note: OpenSSH does **not** support
the `%d` token inside `ProxyCommand`):

```sshconfig
  ProxyCommand ~/.local/share/mise/shims/ts-unplug-proxy connect -fdpass -status 127.0.0.1:8099 %h %p
```

Key flags for `ts-unplug-proxy connect`:
- `-socks5 <addr>` — SOCKS5 proxy address (default `127.0.0.1:1080` when `-http` is unset)
- `-http <addr>` — HTTP CONNECT proxy address (mutually exclusive with `-socks5`; surfaces `Tailscale-Connect-Error` on failure)
- `-fdpass` — pass the connected socket FD to `ssh` (`ProxyUseFdpass yes`) and exit; omit `-fdpass` (and `ProxyUseFdpass`) for standard stdio relay
- `-status <addr>` — optional `/status` listener (e.g. `127.0.0.1:8099`) so `ssh` fails fast with the Tailscale login URL when the proxy is in `NeedsLogin` instead of waiting for `-timeout`
- `-timeout <dur>` — connect and handshake timeout (default `35s`, slightly above the proxy's `30s` `-dial-timeout` so proxy-side errors surface before the client times out; raise this above `-dial-timeout` if you increase it)

### Why `Match final host` instead of `Host`

If your `~/.ssh/config` defines short aliases like:

```sshconfig
Host gxa
  HostName gxa.wild-dojo.ts.net
```

a `Host *.wild-dojo.ts.net` block will **not** match `ssh gxa` because `Host`
only matches the alias typed on the command line, not the expanded `HostName`.
`Match final host *.wild-dojo.ts.net,*.ts.net` triggers OpenSSH's final pass
after `HostName` substitution, so:
- both `ssh gxa` and `ssh gxa.wild-dojo.ts.net` match, even when the `Match`
  block is included near the top of `~/.ssh/config`;
- hosts that already set `ProxyJump` on pass 1 keep their `ProxyJump`;
- non-tailnet hosts (`github.com`, LAN IPs) are unaffected.

### Watch out for raw `100.x` IP addresses

Do **not** glob `100.*` in `Match final host` on machines running Cloudflare
WARP/One (which also allocates addresses inside `100.64.0.0/10`), and note that
OpenSSH `Match host` does not evaluate CIDR notation like `100.64.0.0/10`.
Prefer MagicDNS hostnames (`host.tailnet.ts.net`), or match specific literal
Tailscale IPs explicitly when a host has no MagicDNS name.

### Fallback using `/usr/bin/nc` (macOS)

If you prefer not to invoke `ts-unplug-proxy connect`, macOS's built-in
`/usr/bin/nc` supports SOCKS5 hostname mode (`-X 5 -x`, without `-F` FD passing):

```sshconfig
Match final host *.wild-dojo.ts.net,*.ts.net
  ProxyCommand /usr/bin/nc -X 5 -x 127.0.0.1:1080 %h %p
  ServerAliveInterval 30
```

One-shot form:
```sh
ssh -o ProxyCommand='/usr/bin/nc -X 5 -x 127.0.0.1:1080 %h %p' user@machine.tailnet.ts.net
```

Note: Linux distributions vary widely in `nc` implementations (GNU `netcat`
does not support `-X 5 -x`), which is why `ts-unplug-proxy connect` is the
portable default.

### Connection multiplexing (`ControlMaster`)

Opening each new SSH session over `tsnet` performs a full SSH handshake over
the userspace tunnel. For repeated `ssh`/`scp`/`git`/`rsync` commands, scoping
OpenSSH multiplexing to your tailnet block eliminates that per-command setup
cost:

```sshconfig
Match final host *.wild-dojo.ts.net,*.ts.net
  ProxyCommand ts-unplug-proxy connect -fdpass -status 127.0.0.1:8099 %h %p
  ProxyUseFdpass yes
  ServerAliveInterval 30
  ServerAliveCountMax 3
  ControlMaster auto
  ControlPath ~/.ssh/cm-%C
  ControlPersist 5m
```

Tradeoff: an idle master connection can linger briefly after a proxy restart
until keepalives expire (or until you run `ssh -O exit <host>`). Keep
`ControlPersist` short (or omit it if you prefer fresh dials on every command).

### Tailscale SSH (`tailssh`)

Targets running Tailscale SSH (instead of `sshd`) work transparently through
`ts-unplug-proxy`, including check mode (the target node pushes the verification
URL as an SSH auth banner directly to your terminal). Two caveats apply:

1. **Identity and ACLs:** Without `-advertise-tags`, the `tsnet` node
   authenticates as your user identity (`autogroup:member` / user ACLs). If
   your tailnet SSH policy requires a tagged source, pass
   `-advertise-tags tag:<name>`.
2. **Subnet-routed hosts:** Tailscale SSH only terminates on Tailscale nodes
   themselves; hosts reached behind a subnet router (`-accept-routes`) still
   need a regular `sshd`.

## Examples

### SOCKS5 With curl

Use SOCKS5 hostname mode so the tailnet DNS name is sent to the proxy:
```sh
ts-unplug-proxy -dir ./state -socks5 127.0.0.1:1080
curl --socks5-hostname 127.0.0.1:1080 http://api-staging.tailnet.ts.net
```

For proxy URLs, use `socks5h://` rather than `socks5://` when the client supports it:
```sh
ALL_PROXY=socks5h://127.0.0.1:1080 curl http://api-staging.tailnet.ts.net
```

### HTTP Proxy With curl

```sh
ts-unplug-proxy -dir ./state -http 127.0.0.1:8080
curl -x http://127.0.0.1:8080 http://api-staging.tailnet.ts.net
```

HTTPS through the HTTP proxy uses CONNECT:
```sh
curl -x http://127.0.0.1:8080 https://api-staging.tailnet.ts.net
```

### Browser Proxy

Start one or both proxy listeners:
```sh
ts-unplug-proxy -dir ./state -socks5 127.0.0.1:1080 -http 127.0.0.1:8080
```

Configure your browser to use either SOCKS host `127.0.0.1:1080` or HTTP proxy `127.0.0.1:8080`.

## How It Works

```
┌─────────────────────────────────────────────────────────┐
│  Your Local Machine                                     │
│                                                         │
│  ┌──────────────┐   SOCKS5/HTTP    ┌────────────────┐   │
│  │ Your App     │ ───────────────> │ ts-unplug-     │   │
│  │              │                  │ proxy          │   │
│  └──────────────┘                  └───────┬────────┘   │
│                                            │            │
└────────────────────────────────────────────┼────────────┘
                                             │ Tailscale
                                             │
┌────────────────────────────────────────────┼────────────┐
│  Tailnet                                   │            │
│                                    ┌───────▼────────┐   │
│                                    │ Remote Service │   │
│                                    └────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

ts-unplug-proxy:
1. Connects a `tsnet.Server` to your tailnet
2. Listens locally for SOCKS5 and/or HTTP proxy connections
3. Receives each requested destination from the proxy client
4. Resolves destinations through Tailscale DNS via `LocalClient.QueryDNS`
5. Falls back to MagicDNS peer names from Tailscale status, including short names like `sippy`
6. Falls back to normal `tsnet.Server.Dial` for non-tailnet public names such as `www.google.ca`
7. Dials resolved IPs with `tsnet.Server.Dial`
8. Relays traffic between the client and the destination

The proxy does not use the local OS resolver for tailnet hostnames received from SOCKS5 or HTTP proxy clients. Public names may use the normal resolver path inside `tsnet.Server.Dial`, which is necessary for general internet egress.

## Security Considerations

### Local Binding

Use a loopback listen address such as `127.0.0.1:1080` unless you intentionally want other machines to use the proxy. Non-loopback binds are refused unless `-allow-non-loopback` is passed.

### Authentication

This command does not implement proxy authentication. Do not bind it to a shared interface unless the network path is already trusted and access-controlled.

### State Directory

The `-dir` contains sensitive Tailscale credentials. Protect it appropriately:
```sh
chmod 700 ./state
```

## Troubleshooting

### Missing Listener Error

At least one proxy listener is required:
```sh
ts-unplug-proxy -dir ./state -socks5 127.0.0.1:1080
ts-unplug-proxy -dir ./state -http 127.0.0.1:8080
```

### Tailnet DNS Does Not Resolve With SOCKS5

Use SOCKS5 hostname mode:
```sh
curl --socks5-hostname 127.0.0.1:1080 http://myserver.tailnet.ts.net
```

For proxy URLs, use `socks5h://` rather than `socks5://` when the client supports it:
```sh
ALL_PROXY=socks5h://127.0.0.1:1080 curl http://myserver.tailnet.ts.net
```

### Port Already in Use

```sh
lsof -i :1080
ts-unplug-proxy -dir ./state -socks5 127.0.0.1:1081
```

### Can't Reach Remote Service

1. Verify the remote hostname and port are correct
2. Check the remote service is running
3. Ensure your Tailscale identity has access to the destination
4. Try `tailscale ping myserver.tailnet.ts.net`

## See Also

- [ts-unplug Guide](./ts-unplug.md) - Fixed local port mapping to a tailnet service
- [ts-plug Guide](./ts-plug.md) - Expose local services to your tailnet
- [Main README](../README.md) - Quick start guide
