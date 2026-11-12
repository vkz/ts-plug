package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestStatusHandlerReadiness(t *testing.T) {
	s := &statusState{suffixes: []string{"wild-dojo.ts.net"}, proxies: []string{"PROXY 127.0.0.1:8080"}}

	rec := httptest.NewRecorder()
	statusHandler(s)(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 503 {
		t.Errorf("before ready: code = %d, want 503", rec.Code)
	}
	var before map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if before["ready"] != false {
		t.Errorf("ready = %v, want false", before["ready"])
	}

	s.mu.Lock()
	s.ready = true
	s.state = "Running"
	s.mu.Unlock()

	rec = httptest.NewRecorder()
	statusHandler(s)(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Errorf("after ready: code = %d, want 200", rec.Code)
	}
	var after map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if after["backend_state"] != "Running" || after["ready"] != true {
		t.Errorf("unexpected status body: %v", after)
	}
}
