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
	suffixes []string
	proxies  []string
}

func (s *statusState) snapshot(ctx context.Context) map[string]any {
	s.mu.Lock()
	lc := s.lc
	suffixes := s.suffixes
	proxies := s.proxies
	s.mu.Unlock()

	out := map[string]any{
		"ready":         false,
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
		if ready, _ := snap["ready"].(bool); !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
	}
}
