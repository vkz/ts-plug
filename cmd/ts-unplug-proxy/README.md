# ts-unplug-proxy

Run local SOCKS5/HTTP/PAC proxies, static TCP port forwards (`-L`), and a full
set of tailnet CLI tools (`connect`, `status`, `peers`, `ping`, `login`,
`conns`, `env`, `file`/`cp`, `doctor`) through a single userspace `tsnet.Server`
— ideal on machines where installing `tailscaled` is not possible (for example
behind Cloudflare WARP/One).

## Build

```sh
cd ../..
make ts-unplug-proxy
```

Or install directly (module path is `github.com/vkz/ts-plug`):

```sh
go install github.com/vkz/ts-plug/cmd/ts-unplug-proxy@latest
```

## Quick Examples

```sh
# Full daemon behind Cloudflare WARP (SOCKS5 + HTTP + PAC + static TCP forward)
ts-unplug-proxy -dir ~/.local/state/ts-unplug-proxy \
  -socks5 127.0.0.1:1080 -http 127.0.0.1:8080 \
  -pac-addr 127.0.0.1:8099 \
  -L 127.0.0.1:7687=neo4j.wild-dojo.ts.net:7687 \
  -disable-interface-binding -accept-routes

# Tailnet CLI via the daemon's 0600 Unix control socket (<dir>/control.sock)
ts-unplug-proxy status
ts-unplug-proxy login -open
ts-unplug-proxy peers
ts-unplug-proxy ping gxa
ts-unplug-proxy conns

# One-shot command through the proxy (auto-discovers listeners)
ts-unplug-proxy env -- curl https://gxa.wild-dojo.ts.net/

# Taildrop file send / receive
ts-unplug-proxy cp report.pdf gxa:
ts-unplug-proxy file ls
ts-unplug-proxy file get ~/Downloads

# OpenSSH (~/.ssh/config) — zero lingering helper process via ProxyUseFdpass
Match final host *.wild-dojo.ts.net,*.ts.net
  ProxyCommand ~/.local/share/mise/shims/ts-unplug-proxy connect -fdpass -status 127.0.0.1:8099 %h %p
  ProxyUseFdpass yes
  ServerAliveInterval 30

# Diagnose state, listeners, PAC and routes
ts-unplug-proxy doctor -dir ~/.local/state/ts-unplug-proxy -http 127.0.0.1:8080
```

Run `ts-unplug-proxy -h` or `ts-unplug-proxy help <subcommand>` for full flags and usage.

## Documentation

See **[docs/ts-unplug-proxy.md](../../docs/ts-unplug-proxy.md)** for complete documentation.
