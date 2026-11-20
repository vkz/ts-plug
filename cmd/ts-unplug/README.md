# ts-unplug

Bring Tailscale services to localhost - access remote services as if they were local.

## Build

```sh
cd ../..
make ts-unplug
```

## Usage

```sh
ts-unplug -dir ./state -port 8080 myserver.tailnet-name.ts.net
# Access at http://localhost:8080
```

### Tailnet & macOS Tunnel Flags

- `-advertise-tags <tag:a,tag:b>` — Comma-separated Tailscale ACL tags to advertise on this node (e.g. `tag:dev,tag:proxy`). Needed when your tailnet ACL policy grants access to tag-based sources instead of personal user identities. Invalid tags fail fast before starting `tsnet`.
- `-disable-interface-binding` — Disable Tailscale's macOS network namespace integration (`netns.SetEnabled(false)`). Needed on macOS behind Cloudflare WARP / Cloudflare One (or another tunnel that owns interface and route selection).

```sh
ts-unplug -dir ./state -disable-interface-binding -advertise-tags tag:dev -port 8080 myserver.tailnet-name.ts.net
```

## Documentation

See **[docs/ts-unplug.md](../../docs/ts-unplug.md)** for complete documentation.
