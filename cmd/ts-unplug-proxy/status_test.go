package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
)

func TestStatusHandlerReadiness(t *testing.T) {
	tr := newConnTracker()
	c, peer := net.Pipe()
	defer peer.Close()
	tc := tr.Wrap("socks5", "alpha.ts.net:22", c)
	defer tc.Close()

	s := &statusState{
		tracker:     tr,
		hostname:    "my-proxy",
		suffixes:    []string{"wild-dojo.ts.net"},
		proxies:     []string{"PROXY 127.0.0.1:8080"},
		socks5Addr:  "127.0.0.1:1080",
		httpAddr:    "127.0.0.1:8080",
		controlSock: "/tmp/control.sock",
		forwards:    []string{"127.0.0.1:7687=neo4j.ts.net:7687"},
		tailnetOnly: true,
	}

	// No LocalClient yet and not serving: 503.
	rec := httptest.NewRecorder()
	statusHandler(s, true)(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusServiceUnavailable {
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
	for _, key := range []string{
		"ready", "serving", "hostname", "backend_state", "auth_url", "key_expiry",
		"route_all", "exit_node", "suffixes", "proxies", "socks5_addr", "http_addr",
		"control_sock", "forwards", "tailnet_only", "active_connections", "version",
	} {
		if _, ok := body[key]; !ok {
			t.Errorf("status body missing %q", key)
		}
	}

	var doc statusDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.ActiveConnections != 1 {
		t.Errorf("active_connections = %d, want 1", doc.ActiveConnections)
	}
	if doc.SOCKS5Addr != "127.0.0.1:1080" || doc.HTTPAddr != "127.0.0.1:8080" || doc.ControlSock != "/tmp/control.sock" {
		t.Errorf("addrs mismatch: %+v", doc)
	}
	if !doc.TailnetOnly {
		t.Errorf("tailnet_only = false, want true")
	}
	if len(doc.Forwards) != 1 || doc.Forwards[0] != "127.0.0.1:7687=neo4j.ts.net:7687" {
		t.Errorf("forwards = %v", doc.Forwards)
	}

	// Serving but no client (backend not Running): still 503.
	s.setServing()
	rec = httptest.NewRecorder()
	statusHandler(s, true)(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("serving without client: code = %d, want 503", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["serving"] != true {
		t.Errorf("serving = %v, want true", body["serving"])
	}
}

func TestStatusAuthURLRedactionAndKeyExpiry(t *testing.T) {
	expiry := time.Date(2027, 6, 15, 10, 30, 0, 0, time.UTC)
	s := &statusState{
		hostname: "my-proxy",
		statusFn: func(context.Context) (*ipnstate.Status, error) {
			return &ipnstate.Status{
				BackendState: "NeedsLogin",
				AuthURL:      "https://login.tailscale.com/a/secret-token",
				Self: &ipnstate.PeerStatus{
					HostName:  "live-proxy",
					KeyExpiry: &expiry,
				},
			}, nil
		},
		prefsFn: func(context.Context) (*ipn.Prefs, error) {
			return &ipn.Prefs{RouteAll: true}, nil
		},
	}

	// includeAuthURL = true -> auth_url present.
	rec := httptest.NewRecorder()
	statusHandler(s, true)(rec, httptest.NewRequest("GET", "/status", nil))
	var doc statusDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.AuthURL != "https://login.tailscale.com/a/secret-token" {
		t.Errorf("auth_url = %q, want login URL", doc.AuthURL)
	}
	if doc.KeyExpiry != "2027-06-15T10:30:00Z" {
		t.Errorf("key_expiry = %q, want 2027-06-15T10:30:00Z", doc.KeyExpiry)
	}
	if doc.Hostname != "live-proxy" {
		t.Errorf("hostname = %q, want live-proxy", doc.Hostname)
	}
	if !doc.RouteAll {
		t.Errorf("route_all = false, want true")
	}

	// includeAuthURL = false (non-loopback -pac-addr) -> auth_url redacted.
	rec2 := httptest.NewRecorder()
	statusHandler(s, false)(rec2, httptest.NewRequest("GET", "/status", nil))
	var doc2 statusDocument
	if err := json.Unmarshal(rec2.Body.Bytes(), &doc2); err != nil {
		t.Fatal(err)
	}
	if doc2.AuthURL != "" {
		t.Errorf("auth_url on non-loopback = %q, want empty string", doc2.AuthURL)
	}
	if doc2.KeyExpiry != "2027-06-15T10:30:00Z" {
		t.Errorf("key_expiry = %q, want 2027-06-15T10:30:00Z", doc2.KeyExpiry)
	}
}

func TestStatusReadyAndSetSuffixes(t *testing.T) {
	s := &statusState{
		hostname: "my-proxy",
		statusFn: func(context.Context) (*ipnstate.Status, error) {
			return &ipnstate.Status{
				BackendState: "Running",
				Self:         &ipnstate.PeerStatus{HostName: "my-proxy"},
			}, nil
		},
	}
	s.setServing()
	s.setSuffixes([]string{"wild-dojo.ts.net"})

	rec := httptest.NewRecorder()
	statusHandler(s, true)(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var doc statusDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Ready || !doc.Serving {
		t.Errorf("ready/serving = %v/%v, want true/true", doc.Ready, doc.Serving)
	}
	if doc.KeyExpiry != "" {
		t.Errorf("key_expiry = %q, want empty when KeyExpiry nil", doc.KeyExpiry)
	}
	if len(doc.Suffixes) != 1 || doc.Suffixes[0] != "wild-dojo.ts.net" {
		t.Errorf("suffixes = %v, want [wild-dojo.ts.net]", doc.Suffixes)
	}
}

func TestStatusSnapshotNoClient(t *testing.T) {
	s := &statusState{}
	snap := s.snapshot(context.Background(), true)
	if snap.Ready || snap.Serving {
		t.Errorf("ready/serving = %v/%v, want false/false", snap.Ready, snap.Serving)
	}
}
