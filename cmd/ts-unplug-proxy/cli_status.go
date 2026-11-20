package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

const (
	defaultControlRPCTimeout = 10 * time.Second
	defaultPingCount         = 3
	defaultPingTimeout       = 5 * time.Second
	defaultLoginTimeout      = 5 * time.Minute
	keyExpiryWarnWindow      = 7 * 24 * time.Hour
)

// Testable hooks.
var (
	nowFunc           = time.Now
	openURLFunc       = openURLInBrowser
	loginPollInterval = 500 * time.Millisecond
	pingInterval      = 1 * time.Second
)

const socketResolutionDoc = `SOCKET RESOLUTION
  The daemon's Unix control socket is located using the first non-empty source:
    1. -socket <path>
    2. <dir>/control.sock (from -dir <dir>)
    3. $TS_UNPLUG_PROXY_SOCK
    4. $TS_UNPLUG_PROXY_DIR/control.sock
    5. $XDG_STATE_HOME/ts-unplug-proxy/control.sock
       (or ~/.local/state/ts-unplug-proxy/control.sock)
`

const statusUsageText = `ts-unplug-proxy status - show the running daemon's Tailscale and proxy status.

USAGE
  ts-unplug-proxy status [options]

OPTIONS
  -json             print raw status document as pretty JSON
  -dir <dir>        state directory containing control.sock
  -socket <path>    explicit path to the daemon's Unix control socket

` + socketResolutionDoc + `
EXIT CODES
  0  daemon is Running and serving listeners
  1  daemon is unreachable, not ready, or not yet serving
     (with -json, the decoded status JSON is still printed before exiting 1)
  2  invalid flags or arguments

EXAMPLES
  ts-unplug-proxy status
  ts-unplug-proxy status -dir ~/.local/state/ts-unplug-proxy
  ts-unplug-proxy status -json | jq .active_connections
`

const peersUsageText = `ts-unplug-proxy peers - list tailnet peers from the running daemon.

USAGE
  ts-unplug-proxy peers [options]

OPTIONS
  -json             print peer list as a JSON array
  -online           only show online peers
  -dir <dir>        state directory containing control.sock
  -socket <path>    explicit path to the daemon's Unix control socket

` + socketResolutionDoc + `
EXAMPLES
  ts-unplug-proxy peers
  ts-unplug-proxy peers -online
  ts-unplug-proxy peers -json | jq '.[] | select(.online) | .dns_name'
`

const pingUsageText = `ts-unplug-proxy ping - ping a tailnet peer through the running daemon.

USAGE
  ts-unplug-proxy ping [options] <host-or-ip>

OPTIONS
  -c <N>            number of pings to send (default 3, must be >= 1)
  -type <kind>      ping type: disco, tsmp, icmp, or peerapi (default disco)
  -timeout <dur>    per-ping timeout (default 5s)
  -dir <dir>        state directory containing control.sock
  -socket <path>    explicit path to the daemon's Unix control socket

` + socketResolutionDoc + `
EXIT CODES
  0  all pings succeeded
  1  target not found, daemon unreachable, or one or more pings failed
  2  invalid flags or arguments

EXAMPLES
  ts-unplug-proxy ping gxa
  ts-unplug-proxy ping -c 5 -type tsmp gxa.wild-dojo.ts.net
  ts-unplug-proxy ping -timeout 2s 100.64.0.2
`

const loginUsageText = `ts-unplug-proxy login - authenticate the running daemon with Tailscale.

USAGE
  ts-unplug-proxy login [options]

OPTIONS
  -open             open the authentication URL in the default browser
  -wait             wait until login completes and the backend is Running (default true)
  -timeout <dur>    maximum time to wait for login to complete (default 5m)
  -dir <dir>        state directory containing control.sock
  -socket <path>    explicit path to the daemon's Unix control socket

` + socketResolutionDoc + `
EXIT CODES
  0  already logged in, login completed, or auth URL printed with -wait=false
  1  daemon unreachable, login failed, canceled, or timed out
  2  invalid flags or arguments

EXAMPLES
  ts-unplug-proxy login -open
  ts-unplug-proxy login -wait=false
  ts-unplug-proxy login -dir ~/.local/state/ts-unplug-proxy -timeout 2m
`

const connsUsageText = `ts-unplug-proxy conns - list active upstream connections tracked by the daemon.

USAGE
  ts-unplug-proxy conns [options]

OPTIONS
  -json             print active connections as a JSON array
  -dir <dir>        state directory containing control.sock
  -socket <path>    explicit path to the daemon's Unix control socket

` + socketResolutionDoc + `
EXAMPLES
  ts-unplug-proxy conns
  ts-unplug-proxy conns -json | jq 'length'
`

func runStatus(args []string) int {
	return runStatusIO(args, os.Stdout, os.Stderr)
}

func runStatusIO(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, statusUsageText) }

	asJSON := fs.Bool("json", false, "output status as JSON")
	dir := fs.String("dir", "", "state directory containing control.sock")
	sockFlag := fs.String("socket", "", "path to control Unix socket")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "ts-unplug-proxy status: unexpected argument(s): %s\n", strings.Join(fs.Args(), " "))
		return 2
	}

	sockPath, err := resolveControlSocket(*sockFlag, *dir)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy status: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rpcCtx, cancel := context.WithTimeout(ctx, defaultControlRPCTimeout)
	defer cancel()

	doc, err := fetchControlStatus(rpcCtx, sockPath)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy status: %v\n", err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(doc); err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy status: encode JSON: %v\n", err)
			return 1
		}
	} else {
		writeStatusHuman(stdout, doc, sockPath, nowFunc())
	}

	if doc.Ready && doc.Serving {
		return 0
	}
	return 1
}

func writeStatusHuman(w io.Writer, doc *statusDocument, fallbackSock string, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(label, val string) {
		fmt.Fprintf(tw, "%s:\t%s\n", label, val)
	}

	row("State", formatStatusState(doc))
	if doc.Hostname != "" {
		row("Hostname", doc.Hostname)
	}
	if doc.Version != "" {
		row("Version", doc.Version)
	}
	if doc.AuthURL != "" {
		row("Auth URL", doc.AuthURL)
	}
	if doc.KeyExpiry != "" {
		row("Key expiry", formatKeyExpiry(doc.KeyExpiry, now))
	}
	row("SOCKS5", valueOrDash(doc.SOCKS5Addr, "disabled"))
	row("HTTP", valueOrDash(doc.HTTPAddr, "disabled"))
	controlSock := doc.ControlSock
	if controlSock == "" {
		controlSock = fallbackSock
	}
	row("Control socket", valueOrDash(controlSock, "disabled"))
	if len(doc.Forwards) > 0 {
		row("Forwards", strings.Join(doc.Forwards, ", "))
	} else {
		row("Forwards", "none")
	}
	row("Tailnet-only", fmt.Sprintf("%t", doc.TailnetOnly))
	row("RouteAll", fmt.Sprintf("%t", doc.RouteAll))
	row("ExitNode", valueOrDash(doc.ExitNode, "none"))
	if len(doc.Suffixes) > 0 {
		row("Suffixes", strings.Join(doc.Suffixes, ", "))
	} else {
		row("Suffixes", "none")
	}
	row("Active connections", fmt.Sprintf("%d", doc.ActiveConnections))
	_ = tw.Flush()
}

func formatStatusState(doc *statusDocument) string {
	switch {
	case doc.Ready && doc.Serving:
		return "Running (serving)"
	case doc.Ready && !doc.Serving:
		return "Running (not serving)"
	case doc.BackendState != "":
		return doc.BackendState
	default:
		return "Unknown"
	}
}

func valueOrDash(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func formatKeyExpiry(raw string, now time.Time) string {
	exp, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		exp, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return raw
		}
	}
	delta := exp.Sub(now)
	switch {
	case delta <= 0:
		ago := -delta
		if ago < time.Second {
			return fmt.Sprintf("%s (EXPIRED)", raw)
		}
		return fmt.Sprintf("%s (EXPIRED %s ago)", raw, formatRelativeDuration(ago))
	case delta < keyExpiryWarnWindow:
		return fmt.Sprintf("%s (WARNING: expires in %s)", raw, formatRelativeDuration(delta))
	default:
		return fmt.Sprintf("%s (in %s)", raw, formatRelativeDuration(delta))
	}
}

func formatRelativeDuration(d time.Duration) string {
	if d < time.Minute {
		s := int(d / time.Second)
		if s <= 0 {
			return "<1s"
		}
		return fmt.Sprintf("%ds", s)
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

type peerSummary struct {
	Self           bool     `json:"self,omitempty"`
	Hostname       string   `json:"hostname"`
	DNSName        string   `json:"dns_name"`
	IPs            []string `json:"ips"`
	OS             string   `json:"os"`
	Online         bool     `json:"online"`
	Active         bool     `json:"active"`
	Relay          string   `json:"relay"`
	CurAddr        string   `json:"cur_addr"`
	ExitNode       bool     `json:"exit_node"`
	ExitNodeOption bool     `json:"exit_node_option"`
	Tags           []string `json:"tags"`

	// Internal fields for table formatting.
	peerRelay string
}

func runPeers(args []string) int {
	return runPeersIO(args, os.Stdout, os.Stderr)
}

func runPeersIO(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peers", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, peersUsageText) }

	asJSON := fs.Bool("json", false, "output peers as JSON")
	onlineOnly := fs.Bool("online", false, "show only online peers")
	dir := fs.String("dir", "", "state directory containing control.sock")
	sockFlag := fs.String("socket", "", "path to control Unix socket")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "ts-unplug-proxy peers: unexpected argument(s): %s\n", strings.Join(fs.Args(), " "))
		return 2
	}

	sockPath, err := resolveControlSocket(*sockFlag, *dir)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy peers: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rpcCtx, cancel := context.WithTimeout(ctx, defaultControlRPCTimeout)
	defer cancel()

	lc := controlLocalClient(sockPath)
	st, err := lc.Status(rpcCtx)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy peers: query status via %s: %v\n", sockPath, err)
		return 1
	}

	peers := collectPeerSummaries(st, *onlineOnly)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(peers); err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy peers: encode JSON: %v\n", err)
			return 1
		}
		return 0
	}

	if len(peers) == 0 {
		if *onlineOnly {
			fmt.Fprintln(stdout, "No online peers found.")
		} else {
			fmt.Fprintln(stdout, "No peers found.")
		}
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "IP\tNAME\tOS\tSTATUS\tPATH")
	for _, p := range peers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			peerPrimaryIP(p.IPs),
			peerDisplayName(p),
			valueOrDash(p.OS, "-"),
			peerStatusText(p),
			peerPathText(p),
		)
	}
	_ = tw.Flush()
	return 0
}

func collectPeerSummaries(st *ipnstate.Status, onlineOnly bool) []peerSummary {
	out := []peerSummary{}
	if st == nil {
		return out
	}

	var selfID tailcfg.StableNodeID
	if st.Self != nil {
		selfID = st.Self.ID
		ps := summarizePeer(st.Self, true)
		if !onlineOnly || ps.Online {
			out = append(out, ps)
		}
	}

	for _, peer := range st.Peer {
		if peer == nil || peer.ShareeNode {
			continue
		}
		if selfID != "" && peer.ID == selfID {
			continue
		}
		ps := summarizePeer(peer, false)
		if onlineOnly && !ps.Online {
			continue
		}
		out = append(out, ps)
	}

	slices.SortFunc(out, func(a, b peerSummary) int {
		if a.Self != b.Self {
			if a.Self {
				return -1
			}
			return 1
		}
		if a.Online != b.Online {
			if a.Online {
				return -1
			}
			return 1
		}
		if c := cmp.Compare(strings.ToLower(peerSortName(a)), strings.ToLower(peerSortName(b))); c != 0 {
			return c
		}
		return cmp.Compare(peerPrimaryIP(a.IPs), peerPrimaryIP(b.IPs))
	})
	return out
}

func summarizePeer(p *ipnstate.PeerStatus, isSelf bool) peerSummary {
	ips := make([]string, 0, len(p.TailscaleIPs))
	for _, ip := range p.TailscaleIPs {
		ips = append(ips, ip.Unmap().String())
	}
	tags := []string{}
	if p.Tags != nil && !p.Tags.IsNil() && p.Tags.Len() > 0 {
		tags = p.Tags.AsSlice()
	}
	return peerSummary{
		Self:           isSelf,
		Hostname:       p.HostName,
		DNSName:        strings.TrimSuffix(p.DNSName, "."),
		IPs:            ips,
		OS:             p.OS,
		Online:         p.Online,
		Active:         p.Active,
		Relay:          p.Relay,
		CurAddr:        p.CurAddr,
		ExitNode:       p.ExitNode,
		ExitNodeOption: p.ExitNodeOption,
		Tags:           tags,
		peerRelay:      p.PeerRelay,
	}
}

func peerSortName(p peerSummary) string {
	if p.DNSName != "" {
		return p.DNSName
	}
	return p.Hostname
}

func peerDisplayName(p peerSummary) string {
	name := peerSortName(p)
	if name == "" {
		name = "-"
	}
	if p.Self {
		return name + " (self)"
	}
	return name
}

func peerPrimaryIP(ips []string) string {
	for _, s := range ips {
		if ip, err := netip.ParseAddr(s); err == nil && ip.Is4() {
			return s
		}
	}
	if len(ips) > 0 {
		return ips[0]
	}
	return "-"
}

func peerStatusText(p peerSummary) string {
	status := "offline"
	switch {
	case p.Active:
		status = "active"
	case p.Online:
		status = "online"
	}
	switch {
	case p.ExitNode:
		return status + ", exit node"
	case p.ExitNodeOption:
		return status + ", exit option"
	default:
		return status
	}
}

func peerPathText(p peerSummary) string {
	switch {
	case p.CurAddr != "":
		return fmt.Sprintf("direct (%s)", p.CurAddr)
	case p.peerRelay != "":
		return fmt.Sprintf("peer-relay (%s)", p.peerRelay)
	case p.Relay != "":
		return fmt.Sprintf("relay (%s)", p.Relay)
	default:
		return "-"
	}
}

func runPing(args []string) int {
	return runPingIO(args, os.Stdout, os.Stderr)
}

func runPingIO(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, pingUsageText) }

	count := fs.Int("c", defaultPingCount, "number of pings to send")
	pingTypeRaw := fs.String("type", string(tailcfg.PingDisco), "ping type: disco, tsmp, icmp, or peerapi")
	timeout := fs.Duration("timeout", defaultPingTimeout, "per-ping timeout")
	dir := fs.String("dir", "", "state directory containing control.sock")
	sockFlag := fs.String("socket", "", "path to control Unix socket")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *count < 1 {
		fmt.Fprintln(stderr, "ts-unplug-proxy ping: -c must be >= 1")
		return 2
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "ts-unplug-proxy ping: -timeout must be positive")
		return 2
	}
	pingType, err := parsePingType(*pingTypeRaw)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy ping: %v\n", err)
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "ts-unplug-proxy ping: expected 1 target argument <host-or-ip>, got %d\n", fs.NArg())
		return 2
	}
	target := strings.TrimSpace(fs.Arg(0))
	if target == "" {
		fmt.Fprintln(stderr, "ts-unplug-proxy ping: target host or IP must not be empty")
		return 2
	}

	sockPath, err := resolveControlSocket(*sockFlag, *dir)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy ping: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lc := controlLocalClient(sockPath)
	targetIP, err := resolvePingTarget(ctx, lc, target)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy ping: %v\n", err)
		return 1
	}

	succeeded := 0
	failed := 0
	for i := 0; i < *count; i++ {
		if ctx.Err() != nil {
			break
		}
		pingCtx, cancel := context.WithTimeout(ctx, *timeout)
		res, err := lc.Ping(pingCtx, targetIP, pingType)
		cancel()

		if err != nil {
			failed++
			fmt.Fprintf(stderr, "ping %s failed: %v\n", targetIP, err)
			if ctx.Err() != nil {
				break
			}
		} else if res == nil {
			failed++
			fmt.Fprintf(stderr, "ping %s failed: empty response\n", targetIP)
		} else if res.Err != "" {
			failed++
			fmt.Fprintf(stderr, "ping %s failed: %s\n", targetIP, res.Err)
		} else {
			succeeded++
			fmt.Fprintln(stdout, formatPingResult(res, target, targetIP, pingType))
		}

		if i+1 < *count && pingInterval > 0 {
			timer := time.NewTimer(pingInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}

	if succeeded == *count && failed == 0 {
		return 0
	}
	return 1
}

func parsePingType(raw string) (tailcfg.PingType, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "disco":
		return tailcfg.PingDisco, nil
	case "tsmp":
		return tailcfg.PingTSMP, nil
	case "icmp":
		return tailcfg.PingICMP, nil
	case "peerapi":
		return tailcfg.PingPeerAPI, nil
	default:
		return "", fmt.Errorf("invalid -type %q: must be one of disco, tsmp, icmp, peerapi", raw)
	}
}

func resolvePingTarget(ctx context.Context, lc *local.Client, target string) (netip.Addr, error) {
	trimmed := strings.TrimPrefix(strings.TrimSuffix(target, "]"), "[")
	if ip, err := netip.ParseAddr(trimmed); err == nil {
		return ip.Unmap(), nil
	}

	statusCtx, cancel := context.WithTimeout(ctx, defaultControlRPCTimeout)
	defer cancel()
	st, err := lc.Status(statusCtx)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("query tailnet status to resolve %q: %w", target, err)
	}

	ips := lookupMagicDNS(st, "ip", target)
	if len(ips) == 0 {
		return netip.Addr{}, fmt.Errorf("peer %q not found in tailnet status (run 'ts-unplug-proxy peers' to list nodes)", target)
	}
	for _, ip := range ips {
		if ip.Unmap().Is4() {
			return ip.Unmap(), nil
		}
	}
	return ips[0].Unmap(), nil
}

func formatPingResult(res *ipnstate.PingResult, target string, targetIP netip.Addr, pingType tailcfg.PingType) string {
	node := strings.TrimSuffix(res.NodeName, ".")
	if node == "" {
		node = target
	}
	ipStr := res.NodeIP
	if ipStr == "" {
		ipStr = res.IP
	}
	if ipStr == "" {
		ipStr = targetIP.String()
	}
	var via string
	switch {
	case res.Endpoint != "":
		via = res.Endpoint
	case res.PeerRelay != "":
		via = fmt.Sprintf("peer-relay(%s)", res.PeerRelay)
	case res.DERPRegionCode != "":
		via = fmt.Sprintf("DERP(%s)", res.DERPRegionCode)
	case res.DERPRegionID != 0:
		via = fmt.Sprintf("DERP(%d)", res.DERPRegionID)
	case res.PeerAPIURL != "":
		via = res.PeerAPIURL
	default:
		via = string(pingType)
	}
	return fmt.Sprintf("pong from %s (%s) via %s in %.1fms", node, ipStr, via, res.LatencySeconds*1000)
}

func runLogin(args []string) int {
	return runLoginIO(args, os.Stdout, os.Stderr)
}

func runLoginIO(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, loginUsageText) }

	openBrowser := fs.Bool("open", false, "open authentication URL in browser")
	wait := fs.Bool("wait", true, "wait for login to complete")
	timeout := fs.Duration("timeout", defaultLoginTimeout, "timeout while waiting for login")
	dir := fs.String("dir", "", "state directory containing control.sock")
	sockFlag := fs.String("socket", "", "path to control Unix socket")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "ts-unplug-proxy login: unexpected argument(s): %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "ts-unplug-proxy login: -timeout must be positive")
		return 2
	}

	sockPath, err := resolveControlSocket(*sockFlag, *dir)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy login: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	waitCtx, waitCancel := context.WithTimeout(ctx, *timeout)
	defer waitCancel()

	initCtx, initCancel := context.WithTimeout(waitCtx, defaultControlRPCTimeout)
	doc, err := fetchControlStatus(initCtx, sockPath)
	initCancel()
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy login: %v\n", err)
		return 1
	}

	if isLoggedInStatus(doc) {
		fmt.Fprintln(stdout, formatLoginMessage("Already logged in", doc.Hostname))
		return 0
	}

	authURL := strings.TrimSpace(doc.AuthURL)
	if authURL == "" {
		lc := controlLocalClient(sockPath)
		startCtx, startCancel := context.WithTimeout(waitCtx, defaultControlRPCTimeout)
		err := lc.StartLoginInteractive(startCtx)
		startCancel()
		if err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy login: start interactive login: %v\n", err)
			return 1
		}

		// Poll briefly until the control server delivers an auth URL (or the
		// backend transitions straight to Running).
		doc, err = pollUntilAuthURLOrRunning(waitCtx, sockPath)
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(stderr, "ts-unplug-proxy login: canceled")
			} else if waitCtx.Err() != nil {
				fmt.Fprintf(stderr, "ts-unplug-proxy login: timed out after %s waiting for auth URL\n", *timeout)
			} else {
				fmt.Fprintf(stderr, "ts-unplug-proxy login: %v\n", err)
			}
			return 1
		}
		if isLoggedInStatus(doc) {
			fmt.Fprintln(stdout, formatLoginMessage("Logged in", doc.Hostname))
			return 0
		}
		authURL = strings.TrimSpace(doc.AuthURL)
	}

	fmt.Fprintln(stdout, authURL)

	if *openBrowser {
		if err := openURLFunc(authURL); err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy login: warning: failed to open browser: %v\n", err)
		}
	}

	if !*wait {
		return 0
	}

	fmt.Fprintln(stderr, "Waiting for login to complete...")
	lastAuthURL := authURL
	ticker := time.NewTicker(loginPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(stderr, "ts-unplug-proxy login: canceled")
			return 1
		case <-waitCtx.Done():
			fmt.Fprintf(stderr, "ts-unplug-proxy login: timed out after %s waiting for login\n", *timeout)
			return 1
		case <-ticker.C:
			pollCtx, pollCancel := context.WithTimeout(waitCtx, defaultControlRPCTimeout)
			doc, err = fetchControlStatus(pollCtx, sockPath)
			pollCancel()
			if err != nil {
				if ctx.Err() != nil {
					fmt.Fprintln(stderr, "ts-unplug-proxy login: canceled")
				} else if waitCtx.Err() != nil {
					fmt.Fprintf(stderr, "ts-unplug-proxy login: timed out after %s waiting for login\n", *timeout)
				} else {
					fmt.Fprintf(stderr, "ts-unplug-proxy login: %v\n", err)
				}
				return 1
			}
			if isLoggedInStatus(doc) {
				fmt.Fprintln(stdout, formatLoginMessage("Logged in", doc.Hostname))
				return 0
			}
			if u := strings.TrimSpace(doc.AuthURL); u != "" && u != lastAuthURL {
				lastAuthURL = u
				fmt.Fprintln(stdout, u)
			}
		}
	}
}

func isLoggedInStatus(doc *statusDocument) bool {
	return doc != nil && ((doc.Ready && doc.Serving) || doc.BackendState == "Running")
}

func formatLoginMessage(prefix, hostname string) string {
	if h := strings.TrimSpace(hostname); h != "" {
		return fmt.Sprintf("%s (%s)", prefix, h)
	}
	return prefix
}

func pollUntilAuthURLOrRunning(ctx context.Context, sockPath string) (*statusDocument, error) {
	ticker := time.NewTicker(loginPollInterval)
	defer ticker.Stop()

	for {
		pollCtx, cancel := context.WithTimeout(ctx, defaultControlRPCTimeout)
		doc, err := fetchControlStatus(pollCtx, sockPath)
		cancel()
		if err != nil {
			return nil, err
		}
		if isLoggedInStatus(doc) || strings.TrimSpace(doc.AuthURL) != "" {
			return doc, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func openURLInBrowser(rawURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", rawURL)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", rawURL)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w (%s)", err, msg)
		}
		return err
	}
	return nil
}

func runConns(args []string) int {
	return runConnsIO(args, os.Stdout, os.Stderr)
}

func runConnsIO(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("conns", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, connsUsageText) }

	asJSON := fs.Bool("json", false, "output active connections as JSON")
	dir := fs.String("dir", "", "state directory containing control.sock")
	sockFlag := fs.String("socket", "", "path to control Unix socket")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "ts-unplug-proxy conns: unexpected argument(s): %s\n", strings.Join(fs.Args(), " "))
		return 2
	}

	sockPath, err := resolveControlSocket(*sockFlag, *dir)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy conns: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rpcCtx, cancel := context.WithTimeout(ctx, defaultControlRPCTimeout)
	defer cancel()

	conns, err := fetchControlConns(rpcCtx, sockPath)
	if err != nil {
		fmt.Fprintf(stderr, "ts-unplug-proxy conns: %v\n", err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(conns); err != nil {
			fmt.Fprintf(stderr, "ts-unplug-proxy conns: encode JSON: %v\n", err)
			return 1
		}
		return 0
	}

	if len(conns) == 0 {
		fmt.Fprintln(stdout, "No active connections.")
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPROTO\tTARGET\tAGE\tIN\tOUT")
	for _, c := range conns {
		age := formatDurationShort(time.Duration(c.DurationMS) * time.Millisecond)
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n",
			c.ID,
			valueOrDash(c.Proto, "-"),
			valueOrDash(c.Target, "-"),
			age,
			formatBytes(c.BytesIn),
			formatBytes(c.BytesOut),
		)
	}
	_ = tw.Flush()
	return 0
}

func fetchControlConns(ctx context.Context, sockPath string) ([]connSnapshot, error) {
	client := controlHTTPClient(sockPath)
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, controlBaseURL+"/debug/connections", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to control socket %s (is ts-unplug-proxy running?): %w", sockPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("control /debug/connections returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var conns []connSnapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&conns); err != nil {
		return nil, fmt.Errorf("decode control /debug/connections: %w", err)
	}
	if conns == nil {
		conns = []connSnapshot{}
	}
	return conns, nil
}

func formatDurationShort(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	if d < time.Hour {
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	}
	if d < 24*time.Hour {
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := int(d / (24 * time.Hour))
	h := int((d % (24 * time.Hour)) / time.Hour)
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, h)
}

func formatBytes(b int64) string {
	if b <= 0 {
		return "0 B"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	val := float64(b)
	idx := -1
	for val >= unit && idx+1 < len(units) {
		val /= unit
		idx++
	}
	return fmt.Sprintf("%.1f %s", val, units[idx])
}
