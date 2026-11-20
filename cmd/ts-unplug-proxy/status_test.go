package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestStatusHandlerReadiness(t *testing.T) {
	s := &statusState{hostname: "my-proxy", suffixes: []string{"wild-dojo.ts.net"}, proxies: []string{"PROXY 127.0.0.1:8080"}}

	// No LocalClient yet and not serving: 503.
	rec := httptest.NewRecorder()
	statusHandler(s)(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 503 {
		t.Errorf("before client: code = %d, want 503", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ready"] != false || body["serving"] != false {
		t.Errorf("ready/serving = %v/%v, want false/false", body["ready"], body["serving"])
	}
	if body["hostname"] != "my-proxy" {
		t.Errorf("hostname = %v, want my-proxy", body["hostname"])
	}
	for _, key := range []string{"hostname", "backend_state", "auth_url", "route_all", "exit_node", "suffixes", "proxies", "version"} {
		if _, ok := body[key]; !ok {
			t.Errorf("status body missing %q", key)
		}
	}

	// Serving but no client (backend not Running): still 503.
	s.setServing()
	rec = httptest.NewRecorder()
	statusHandler(s)(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 503 {
		t.Errorf("serving without client: code = %d, want 503", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["serving"] != true {
		t.Errorf("serving = %v, want true", body["serving"])
	}
}

func TestStatusSnapshotNoClient(t *testing.T) {
	s := &statusState{}
	snap := s.snapshot(context.Background())
	if snap["ready"] != false || snap["serving"] != false {
		t.Errorf("ready/serving = %v/%v, want false/false", snap["ready"], snap["serving"])
	}
}
