package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestStatusHandlerReadiness(t *testing.T) {
	// No LocalClient yet: not ready, so the health check returns 503.
	s := &statusState{suffixes: []string{"wild-dojo.ts.net"}, proxies: []string{"PROXY 127.0.0.1:8080"}}

	rec := httptest.NewRecorder()
	statusHandler(s)(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 503 {
		t.Errorf("before client: code = %d, want 503", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ready"] != false {
		t.Errorf("ready = %v, want false", body["ready"])
	}
	for _, key := range []string{"backend_state", "auth_url", "route_all", "exit_node", "suffixes", "proxies", "version"} {
		if _, ok := body[key]; !ok {
			t.Errorf("status body missing %q", key)
		}
	}
}

func TestStatusSnapshotNoClient(t *testing.T) {
	s := &statusState{}
	snap := s.snapshot(context.Background())
	if snap["ready"] != false {
		t.Errorf("ready = %v, want false", snap["ready"])
	}
}
