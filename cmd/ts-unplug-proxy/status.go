package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"tailscale.com/client/local"
)

// statusState is the live state reported by GET /status on the PAC listener.
// It is populated before tsnet starts (ready=false) and refreshed once the
// node is up, so a health check can distinguish "listening" from "connected".
type statusState struct {
	mu       sync.Mutex
	ready    bool
	state    string
	exitNode string
	routeAll bool
	suffixes []string
	proxies  []string
}

func (s *statusState) setReady(ctx context.Context, backendState string, lc *local.Client) {
	s.mu.Lock()
	s.ready = true
	s.state = backendState
	s.mu.Unlock()
	if lc == nil {
		return
	}
	prefs, err := lc.GetPrefs(ctx)
	if err != nil {
		return
	}
	exit := string(prefs.ExitNodeID)
	if prefs.AutoExitNode != "" {
		exit = "auto:" + string(prefs.AutoExitNode)
	}
	s.mu.Lock()
	s.routeAll = prefs.RouteAll
	s.exitNode = exit
	s.mu.Unlock()
}

func (s *statusState) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"ready":         s.ready,
		"backend_state": s.state,
		"route_all":     s.routeAll,
		"exit_node":     s.exitNode,
		"suffixes":      s.suffixes,
		"proxies":       s.proxies,
		"version":       version,
	}
}

func statusHandler(s *statusState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := s.snapshot()
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
