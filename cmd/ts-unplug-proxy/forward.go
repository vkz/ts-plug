package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// forwardSpec represents one -L <local-addr>=<remote-host:port> TCP port forward.
type forwardSpec struct {
	local  string
	remote string
}

func (f forwardSpec) String() string {
	return f.local + "=" + f.remote
}

// forwardList implements flag.Value for repeatable -L flags.
type forwardList []forwardSpec

func (l *forwardList) String() string {
	if l == nil || len(*l) == 0 {
		return ""
	}
	parts := make([]string, len(*l))
	for i, f := range *l {
		parts[i] = f.String()
	}
	return strings.Join(parts, ",")
}

func (l *forwardList) Set(val string) error {
	spec, err := parseForwardSpec(val)
	if err != nil {
		return err
	}
	wantKey := canonicalHostPort(spec.local)
	for _, existing := range *l {
		if canonicalHostPort(existing.local) == wantKey {
			return fmt.Errorf("duplicate -L local bind address %q", spec.local)
		}
	}
	*l = append(*l, spec)
	return nil
}

func (l forwardList) strings() []string {
	out := make([]string, len(l))
	for i, f := range l {
		out[i] = f.String()
	}
	return out
}

// parseForwardSpec parses and validates "<local-addr>=<remote-host:port>".
// Both sides must be valid host:port with non-empty host and port in 1..65535.
func parseForwardSpec(raw string) (forwardSpec, error) {
	localRaw, remoteRaw, ok := strings.Cut(raw, "=")
	if !ok {
		return forwardSpec{}, fmt.Errorf("invalid -L %q: expected <local-addr>=<remote-host:port>", raw)
	}
	localAddr := strings.TrimSpace(localRaw)
	remoteAddr := strings.TrimSpace(remoteRaw)
	if strings.Contains(remoteAddr, "=") {
		return forwardSpec{}, fmt.Errorf("invalid -L %q: expected single '=' separating local and remote", raw)
	}
	if err := validateForwardHostPort(localAddr); err != nil {
		return forwardSpec{}, fmt.Errorf("invalid -L local address %q: %w", localAddr, err)
	}
	if err := validateForwardHostPort(remoteAddr); err != nil {
		return forwardSpec{}, fmt.Errorf("invalid -L remote address %q: %w", remoteAddr, err)
	}
	return forwardSpec{local: localAddr, remote: remoteAddr}, nil
}

func validateForwardHostPort(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" {
		return fmt.Errorf("host must not be empty")
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return fmt.Errorf("host must not contain whitespace")
	}
	if strings.ContainsAny(portStr, " \t\r\n") {
		return fmt.Errorf("port must not contain whitespace")
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("port %q must be 1..65535", portStr)
	}
	return nil
}

func canonicalHostPort(addr string) string {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.ToLower(addr)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
	} else {
		host = strings.ToLower(host)
	}
	if port, err := strconv.ParseUint(portStr, 10, 16); err == nil {
		portStr = strconv.FormatUint(port, 10)
	}
	return net.JoinHostPort(host, portStr)
}

// startForward accepts connections on an already-bound listener and relays each
// one to spec.remote via dialer using full-duplex half-close semantics.
func startForward(ctx context.Context, listener net.Listener, spec forwardSpec, dialer func(context.Context, string, string) (net.Conn, error), verbose bool, serveErr chan<- serveResult) {
	name := "forward:" + spec.local
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				serveErr <- serveResult{name: name, err: err}
				return
			}
			if verbose {
				slog.Debug("forward connection accepted",
					slog.String("local", spec.local),
					slog.String("remote", spec.remote),
					slog.String("peer", client.RemoteAddr().String()),
				)
			}
			go handleForwardConn(ctx, client, spec.remote, dialer)
		}
	}()
}

func handleForwardConn(ctx context.Context, client net.Conn, remote string, dialer func(context.Context, string, string) (net.Conn, error)) {
	defer client.Close()
	backend, err := dialer(ctx, "tcp", remote)
	if err != nil {
		slog.Debug("forward dial failed",
			slog.String("remote", remote),
			slog.Any("error", err),
		)
		return
	}
	defer backend.Close()
	relayConn(client, backend, client)
}
