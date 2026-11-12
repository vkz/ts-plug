# AGENTS.md

## Upstream PRs are prohibited

Do **not** open pull requests against `tailscale/ts-plug` or any other external
repository. This is a private fork; all work stays here.

## Where work goes

- Push directly to `main` on the `vkz` remote. Do not wait for review, and do
  not use PRs.
- `main` is the only branch and the fork's default. If you need to isolate a
  change, use a short-lived branch and merge it into `main` before finishing.

## Notes

- `cmd/ts-unplug-proxy` is a userspace `tsnet` SOCKS5/HTTP/PAC proxy for
  machines where the kernel Tailscale client cannot run (another tunnel owns the
  default route, e.g. Cloudflare WARP).
- The module path is `github.com/vkz/ts-plug` so mise's `go:` backend can
  `go install github.com/vkz/ts-plug/cmd/ts-unplug-proxy@<version>` without a
  local checkout.
