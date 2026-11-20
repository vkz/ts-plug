package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"tailscale.com/client/local"
)

// statusState backs GET /status. The backend state is read live from the
// LocalClient on every request (via StatusWithoutPeers), so the endpoint
// reports NeedsLogin/Stopped and the auth URL rather than a cached snapshot.
type statusState struct {
	mu       sync.Mutex
	lc       *local.Client
	hostname string
	serving  bool
	suffixes []string
	proxies  []string
}

// setServing marks the SOCKS/HTTP proxies as accepting. /status only reports
// HTTP 200 once the backend is Running AND this is set, so a health check does
// not pass while listeners are bound but not yet serving.
func (s *statusState) setServing() {
	s.mu.Lock()
	s.serving = true
	s.mu.Unlock()
}

func (s *statusState) snapshot(ctx context.Context) map[string]any {
	s.mu.Lock()
	lc := s.lc
	hostname := s.hostname
	serving := s.serving
	suffixes := s.suffixes
	proxies := s.proxies
	s.mu.Unlock()

	out := map[string]any{
		"ready":         false,
		"serving":       serving,
		"hostname":      hostname,
		"backend_state": "",
		"auth_url":      "",
		"route_all":     false,
		"exit_node":     "",
		"suffixes":      suffixes,
		"proxies":       proxies,
		"version":       version,
	}
	if lc == nil {
		return out
	}
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return out
	}
	out["backend_state"] = st.BackendState
	out["ready"] = st.BackendState == "Running"
	out["auth_url"] = st.AuthURL
	if st.Self != nil && st.Self.HostName != "" {
		out["hostname"] = st.Self.HostName
	}
	if prefs, err := lc.GetPrefs(ctx); err == nil {
		exit := string(prefs.ExitNodeID)
		if prefs.AutoExitNode != "" {
			exit = "auto:" + string(prefs.AutoExitNode)
		}
		out["route_all"] = prefs.RouteAll
		out["exit_node"] = exit
	}
	return out
}

func statusHandler(s *statusState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := s.snapshot(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		ready, _ := snap["ready"].(bool)
		serving, _ := snap["serving"].(bool)
		if !ready || !serving {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
	}
}
