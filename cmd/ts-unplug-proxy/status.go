package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
)

// statusDocument is the JSON schema returned by GET /status (both on the Unix
// control socket and on -pac-addr).
type statusDocument struct {
	Ready             bool     `json:"ready"`
	Serving           bool     `json:"serving"`
	Hostname          string   `json:"hostname"`
	BackendState      string   `json:"backend_state"`
	AuthURL           string   `json:"auth_url"`
	KeyExpiry         string   `json:"key_expiry"`
	RouteAll          bool     `json:"route_all"`
	ExitNode          string   `json:"exit_node"`
	Suffixes          []string `json:"suffixes"`
	Proxies           []string `json:"proxies"`
	SOCKS5Addr        string   `json:"socks5_addr"`
	HTTPAddr          string   `json:"http_addr"`
	ControlSock       string   `json:"control_sock"`
	Forwards          []string `json:"forwards"`
	TailnetOnly       bool     `json:"tailnet_only"`
	ActiveConnections int      `json:"active_connections"`
	Version           string   `json:"version"`
}

// statusState backs GET /status. The backend state is read live from the
// LocalClient on every request (via StatusWithoutPeers), so the endpoint
// reports NeedsLogin/Stopped and the auth URL rather than a cached snapshot.
type statusState struct {
	mu          sync.Mutex
	lc          *local.Client
	tracker     *connTracker
	hostname    string
	serving     bool
	suffixes    []string
	proxies     []string
	socks5Addr  string
	httpAddr    string
	controlSock string
	forwards    []string
	tailnetOnly bool

	// Optional hooks for unit testing without a live tsnet backend.
	statusFn func(context.Context) (*ipnstate.Status, error)
	prefsFn  func(context.Context) (*ipn.Prefs, error)
}

// setServing marks the SOCKS/HTTP/forward listeners as accepting. /status only
// reports HTTP 200 once the backend is Running AND this is set, so a health
// check does not pass while listeners are bound but not yet serving.
func (s *statusState) setServing() {
	s.mu.Lock()
	s.serving = true
	s.mu.Unlock()
}

// setSuffixes atomically updates the reported tailnet DNS suffixes after
// dynamic MagicDNS discovery.
func (s *statusState) setSuffixes(suffixes []string) {
	s.mu.Lock()
	s.suffixes = slices.Clone(suffixes)
	s.mu.Unlock()
}

func (s *statusState) setControlSock(sockPath string) {
	s.mu.Lock()
	s.controlSock = sockPath
	s.mu.Unlock()
}

func (s *statusState) snapshot(ctx context.Context, includeAuthURL bool) statusDocument {
	s.mu.Lock()
	lc := s.lc
	tracker := s.tracker
	statusFn := s.statusFn
	prefsFn := s.prefsFn
	out := statusDocument{
		Serving:     s.serving,
		Hostname:    s.hostname,
		Suffixes:    nonNilStrings(s.suffixes),
		Proxies:     nonNilStrings(s.proxies),
		SOCKS5Addr:  s.socks5Addr,
		HTTPAddr:    s.httpAddr,
		ControlSock: s.controlSock,
		Forwards:    nonNilStrings(s.forwards),
		TailnetOnly: s.tailnetOnly,
		Version:     version,
	}
	s.mu.Unlock()

	if tracker != nil {
		out.ActiveConnections = tracker.Count()
	}

	var st *ipnstate.Status
	var err error
	switch {
	case statusFn != nil:
		st, err = statusFn(ctx)
	case lc != nil:
		st, err = lc.StatusWithoutPeers(ctx)
	default:
		return out
	}
	if err != nil || st == nil {
		return out
	}

	out.BackendState = st.BackendState
	out.Ready = st.BackendState == "Running"
	if includeAuthURL {
		out.AuthURL = st.AuthURL
	}
	if st.Self != nil {
		if st.Self.HostName != "" {
			out.Hostname = st.Self.HostName
		}
		if st.Self.KeyExpiry != nil && !st.Self.KeyExpiry.IsZero() {
			out.KeyExpiry = st.Self.KeyExpiry.UTC().Format(time.RFC3339)
		}
	}

	var prefs *ipn.Prefs
	var prefsErr error
	switch {
	case prefsFn != nil:
		prefs, prefsErr = prefsFn(ctx)
	case lc != nil:
		prefs, prefsErr = lc.GetPrefs(ctx)
	}
	if prefsErr == nil && prefs != nil {
		exit := string(prefs.ExitNodeID)
		if prefs.AutoExitNode != "" {
			exit = "auto:" + string(prefs.AutoExitNode)
		}
		out.RouteAll = prefs.RouteAll
		out.ExitNode = exit
	}
	return out
}

func nonNilStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	return slices.Clone(in)
}

func statusHandler(s *statusState, includeAuthURL bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := s.snapshot(r.Context(), includeAuthURL)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if !snap.Ready || !snap.Serving {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
	}
}
