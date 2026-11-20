package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	defaultEnvSOCKS5        = "127.0.0.1:1080"
	envControlStatusTimeout = 2 * time.Second
)

var defaultNoProxyEntries = []string{"localhost", "127.0.0.1", "::1"}

const envUsageText = `ts-unplug-proxy env - run a command with proxy environment variables pointing
at a local ts-unplug-proxy listener, or print POSIX shell exports.

USAGE
  ts-unplug-proxy env [options] -- <command> [args...]
  ts-unplug-proxy env [options] <command> [args...]
  ts-unplug-proxy env [options] -print

  Note: 'env' must be the first argument after ts-unplug-proxy.

OPTIONS
  -socks5 <addr>    SOCKS5 proxy host:port override (sets ALL_PROXY and
                    all_proxy to socks5h://<addr>)
  -http <addr>      HTTP proxy host:port override (sets HTTP_PROXY, http_proxy,
                    HTTPS_PROXY, and https_proxy to http://<addr>; also sets
                    ALL_PROXY/all_proxy when -socks5 is unset)
  -no-proxy <list>  comma-separated extra NO_PROXY entries, merged and
                    de-duplicated with localhost,127.0.0.1,::1 and any
                    existing NO_PROXY/no_proxy environment variables
  -print            print POSIX shell 'export ...' statements to stdout instead
                    of executing a command (properly single-quoted for eval)
  -dir <dir>        state dir to locate the running daemon's control.sock for
                    auto-discovering listeners
  -socket <path>    explicit control socket path for auto-discovering listeners

AUTO-DISCOVERY
  When neither -socks5 nor -http is specified, 'env' queries the running
  daemon's control socket (/status) to discover its configured listeners.
  If -socket or -dir is explicitly specified and the daemon is unreachable,
  'env' exits with an error. Otherwise, if the default control socket is
  unavailable, it falls back to socks5h://127.0.0.1:1080 with a note on stderr.

ENVIRONMENT VARIABLES SET
  When SOCKS5 is configured:
    ALL_PROXY, all_proxy = socks5h://<socks5>
  When HTTP is configured:
    HTTP_PROXY, http_proxy, HTTPS_PROXY, https_proxy = http://<http>
    (and ALL_PROXY, all_proxy = http://<http> when SOCKS5 is not configured)
  Always:
    NO_PROXY, no_proxy = localhost,127.0.0.1,::1[,<existing>][,<extra>]

NOTE ON -tailnet-only
  Warning: when the ts-unplug-proxy daemon is started with -tailnet-only,
  commands invoked via 'env' will have non-tailnet traffic rejected by the
  proxy. Add non-tailnet hosts to -no-proxy if they should bypass the proxy.

EXAMPLES
  # Auto-discover listeners from the running daemon and run curl:
  ts-unplug-proxy env -- curl https://gxa.wild-dojo.ts.net/

  # Run a command without '--':
  ts-unplug-proxy env git clone https://git.wild-dojo.ts.net/org/repo.git

  # Export proxy variables into the current shell:
  eval "$(ts-unplug-proxy env -print)"

  # Override proxy addresses explicitly (no control socket required):
  ts-unplug-proxy env -socks5 127.0.0.1:1080 -http 127.0.0.1:8080 -- curl -I https://gxa.wild-dojo.ts.net/

  # Add extra NO_PROXY exclusions:
  ts-unplug-proxy env -no-proxy ".corp.internal,10.0.0.0/8" -- ./my-tool
`

type envExecFunc func(cmdArgs []string, env []string) (int, error)

func runEnv(args []string) int {
	return runEnvIO(args, os.Environ(), os.Stdout, os.Stderr, execCommand)
}

func runEnvIO(args []string, environ []string, stdout, stderr io.Writer, execFn envExecFunc) int {
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = io.WriteString(stderr, envUsageText)
	}

	socksFlag := fs.String("socks5", "", "SOCKS5 proxy host:port override")
	httpFlag := fs.String("http", "", "HTTP proxy host:port override")
	noProxyFlag := fs.String("no-proxy", "", "comma-separated extra NO_PROXY entries")
	printFlag := fs.Bool("print", false, "print POSIX shell export statements to stdout")
	dirFlag := fs.String("dir", "", "state dir to locate running daemon's control.sock")
	sockFlag := fs.String("socket", "", "explicit control socket path")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cmdArgs := fs.Args()
	if *printFlag {
		if len(cmdArgs) > 0 {
			fmt.Fprintln(stderr, "ts-unplug-proxy env: -print cannot be used with a command")
			return 2
		}
	} else if len(cmdArgs) == 0 {
		fmt.Fprintln(stderr, "ts-unplug-proxy env: missing command (pass -- <command> [args...] or -print; run 'ts-unplug-proxy env -h' for usage)")
		return 2
	}

	socksAddr := strings.TrimSpace(*socksFlag)
	httpAddr := strings.TrimSpace(*httpFlag)

	if socksAddr == "" && httpAddr == "" {
		var err error
		socksAddr, httpAddr, err = discoverEnvProxyAddrs(*sockFlag, *dirFlag, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy env: %v\n", err)
			return 1
		}
	}

	proxyEnv := buildProxyEnv(socksAddr, httpAddr, *noProxyFlag, environ)

	if *printFlag {
		writeShellExports(stdout, proxyEnv)
		return 0
	}

	if execFn == nil {
		execFn = execCommand
	}
	merged := mergeEnv(environ, proxyEnv)
	code, err := execFn(cmdArgs, merged)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy env: %v\n", err)
	}
	return code
}

func discoverEnvProxyAddrs(flagSock, flagDir string, stderr io.Writer) (socksAddr, httpAddr string, err error) {
	explicitTarget := strings.TrimSpace(flagSock) != "" || strings.TrimSpace(flagDir) != ""

	sockPath, err := resolveControlSocket(flagSock, flagDir)
	if err != nil {
		if explicitTarget {
			return "", "", err
		}
		fmt.Fprintf(stderr, "ts-unplug-proxy env: control socket unavailable, defaulting to socks5h://%s (pass -socks5/-http or start daemon)\n", defaultEnvSOCKS5)
		return defaultEnvSOCKS5, "", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), envControlStatusTimeout)
	defer cancel()

	doc, err := fetchControlStatus(ctx, sockPath)
	if err != nil {
		if explicitTarget {
			return "", "", err
		}
		fmt.Fprintf(stderr, "ts-unplug-proxy env: control socket unavailable, defaulting to socks5h://%s (pass -socks5/-http or start daemon)\n", defaultEnvSOCKS5)
		return defaultEnvSOCKS5, "", nil
	}

	if s := strings.TrimSpace(doc.SOCKS5Addr); s != "" {
		socksAddr = pacAdvertiseAddr(s)
	}
	if h := strings.TrimSpace(doc.HTTPAddr); h != "" {
		httpAddr = pacAdvertiseAddr(h)
	}
	if socksAddr == "" && httpAddr == "" {
		return "", "", fmt.Errorf("running daemon at %s has neither -socks5 nor -http configured (start the daemon with -socks5 or -http, or pass -socks5/-http explicitly)", sockPath)
	}
	return socksAddr, httpAddr, nil
}

// buildProxyEnv constructs the proxy environment variables for the given
// SOCKS5 and HTTP proxy addresses and merges NO_PROXY entries from defaults,
// existing environment variables, and extraNoProxy.
func buildProxyEnv(socksAddr, httpAddr, extraNoProxy string, environ []string) map[string]string {
	out := make(map[string]string, 8)
	if socksAddr != "" {
		socksURL := "socks5h://" + socksAddr
		out["ALL_PROXY"] = socksURL
		out["all_proxy"] = socksURL
	}
	if httpAddr != "" {
		httpURL := "http://" + httpAddr
		out["HTTP_PROXY"] = httpURL
		out["http_proxy"] = httpURL
		out["HTTPS_PROXY"] = httpURL
		out["https_proxy"] = httpURL
		if socksAddr == "" {
			out["ALL_PROXY"] = httpURL
			out["all_proxy"] = httpURL
		}
	}

	noProxy := mergeNoProxy(environ, extraNoProxy)
	out["NO_PROXY"] = noProxy
	out["no_proxy"] = noProxy
	return out
}

func mergeNoProxy(environ []string, extraNoProxy string) string {
	var existingUpper, existingLower string
	for _, entry := range environ {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch k {
		case "NO_PROXY":
			existingUpper = v
		case "no_proxy":
			existingLower = v
		}
	}

	seen := make(map[string]bool)
	var merged []string
	add := func(items []string) {
		for _, item := range items {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			key := strings.ToLower(item)
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, item)
		}
	}

	add(defaultNoProxyEntries)
	add(splitCommaList(existingUpper))
	add(splitCommaList(existingLower))
	add(splitCommaList(extraNoProxy))

	return strings.Join(merged, ",")
}

// mergeEnv returns a new environment slice with overrides applied over base.
// Matching keys (case-sensitive) are replaced without duplicates, unrelated
// entries are preserved in order, and new keys are appended in sorted order.
func mergeEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	seen := make(map[string]bool, len(overrides))

	for _, entry := range base {
		k, _, ok := strings.Cut(entry, "=")
		if ok {
			if val, exists := overrides[k]; exists {
				if !seen[k] {
					out = append(out, k+"="+val)
					seen[k] = true
				}
				continue
			}
		}
		out = append(out, entry)
	}

	var remaining []string
	for k := range overrides {
		if !seen[k] {
			remaining = append(remaining, k)
		}
	}
	slices.Sort(remaining)
	for _, k := range remaining {
		out = append(out, k+"="+overrides[k])
	}
	return out
}

func writeShellExports(w io.Writer, env map[string]string) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "export %s=%s\n", k, shellSingleQuote(env[k]))
	}
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
