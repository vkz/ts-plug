# ts-unplug-proxy Guide

**ts-unplug-proxy** runs local SOCKS5 and/or HTTP proxies that dial requested destinations through a `tsnet.Server`.

## Overview

ts-unplug-proxy allows you to:
- Use one local proxy process for many tailnet services
- Let proxy-aware tools choose the remote hostname and port
- Resolve tailnet hostnames through Tailscale status/DNS instead of local DNS
- Avoid running one fixed local port mapping per remote service

This differs from `ts-unplug`, which maps one local TCP port to one fixed remote target.

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

### Optional

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

- `-pac-addr <addr>` - Serve `/proxy.pac` on this address (loopback only). Browsers that use the PAC send only tailnet hosts through the proxy.

- `-pac-domain <a,b>` - Comma-separated tailnet DNS suffixes the PAC must never send `DIRECT`. Defaults to the MagicDNS name recorded in `-dir`.

- `-exit-node` - Use a tailnet exit node for non-tailnet egress. Off by default. See [Exit nodes and persisted prefs](#exit-nodes-and-persisted-prefs).

- `-clear-exit-node` - One-shot: clear a persisted exit-node preference from `-dir`, then exit. Stop the proxy first.

- `-direct` - Force non-tailnet names through the OS resolver and dialer. Off by default and rarely needed: without an exit node, non-tailnet traffic already egresses via the OS. Does not apply to IP literals.

- `-allow-non-loopback` - Allow `-socks5`/`-http`/`-pac-addr` to bind non-loopback addresses. Off by default.

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
than leaking. `GET /status` on the same address returns a JSON health document
(`ready`, `backend_state`, `route_all`, `exit_node`, `suffixes`, `proxies`,
`version`).

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
when it can take the state-dir lock (i.e. the proxy is not running).

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
