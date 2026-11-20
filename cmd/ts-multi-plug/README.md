# ts-plug

Expose local services to your tailnet with automatic TLS and DNS.

## Build

```sh
cd ../..
make ts-plug
```

## Usage

```sh
ts-plug -hostname myapp -- python -m http.server 8080
# Access at https://myapp.tailnet-name.ts.net
```

### Tailnet & macOS Tunnel Flags

- `-advertise-tags <tag:a,tag:b>` — Comma-separated Tailscale ACL tags to advertise on this node (e.g. `tag:dev,tag:web`). Needed when your tailnet ACL policy targets tagged devices instead of personal user identities. Invalid tags fail fast before starting the child command.
- `-disable-interface-binding` — Disable Tailscale's macOS network namespace integration (`netns.SetEnabled(false)`). Needed on macOS behind Cloudflare WARP / Cloudflare One (or another tunnel that owns interface and route selection).

```sh
ts-plug -disable-interface-binding -advertise-tags tag:dev -hostname myapp -- python -m http.server 8080
```

## Documentation

See **[docs/ts-plug.md](../../docs/ts-plug.md)** for complete documentation.
