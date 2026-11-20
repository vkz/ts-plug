package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"

	"tailscale.com/client/local"
)

const controlBaseURL = "http://ts-unplug-proxy"

// newControlMux builds the HTTP handler mounted on the Unix control socket:
//   - /localapi/         -> reverse proxy to tsnet's LocalAPI (lc.Dial)
//   - /status            -> full JSON status document (including auth_url)
//   - /debug/connections -> JSON array of active tracked connections
func newControlMux(localDial func(context.Context, string, string) (net.Conn, error), status *statusState, tracker *connTracker) http.Handler {
	mux := http.NewServeMux()

	if localDial != nil {
		rp := &httputil.ReverseProxy{
			FlushInterval: -1,
			Transport: &http.Transport{
				DialContext: localDial,
			},
			Director: func(req *http.Request) {
				req.URL.Scheme = "http"
				req.URL.Host = "local-tailscaled.sock"
				req.Host = "local-tailscaled.sock"
				req.Header.Del("Origin")
				req.Header.Del("Referer")
			},
		}
		mux.Handle("/localapi/", rp)
	} else {
		mux.HandleFunc("/localapi/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "tsnet localapi unavailable", http.StatusServiceUnavailable)
		})
	}

	mux.HandleFunc("/status", statusHandler(status, true))
	mux.HandleFunc("/debug/connections", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		snap := tracker.Snapshot()
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
	})

	return mux
}

// startControlServer serves the control HTTP mux on an already-bound Unix
// socket listener.
func startControlServer(ctx context.Context, listener net.Listener, localDial func(context.Context, string, string) (net.Conn, error), status *statusState, tracker *connTracker, serveErr chan<- serveResult) {
	server := &http.Server{
		Handler:     newControlMux(localDial, status, tracker),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		serveErr <- serveResult{name: "control", err: server.Serve(listener)}
	}()
}

// defaultStateDir returns the default state directory for CLI subcommands:
// $TS_UNPLUG_PROXY_DIR, or ${XDG_STATE_HOME:-~/.local/state}/ts-unplug-proxy.
func defaultStateDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("TS_UNPLUG_PROXY_DIR")); d != "" {
		return d, nil
	}
	stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("determine home directory for default state dir: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "ts-unplug-proxy"), nil
}

// resolveControlSocket locates the control Unix socket using the precedence:
//  1. flagSock (if non-empty)
//  2. filepath.Join(flagDir, "control.sock") (if flagDir non-empty)
//  3. $TS_UNPLUG_PROXY_SOCK
//  4. filepath.Join($TS_UNPLUG_PROXY_DIR, "control.sock")
//  5. filepath.Join($XDG_STATE_HOME or ~/.local/state, "ts-unplug-proxy", "control.sock")
func resolveControlSocket(flagSock, flagDir string) (string, error) {
	if s := strings.TrimSpace(flagSock); s != "" {
		return s, nil
	}
	if d := strings.TrimSpace(flagDir); d != "" {
		return filepath.Join(d, "control.sock"), nil
	}
	if s := strings.TrimSpace(os.Getenv("TS_UNPLUG_PROXY_SOCK")); s != "" {
		return s, nil
	}
	dir, err := defaultStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "control.sock"), nil
}

// controlLocalClient returns a Tailscale LocalAPI client pointed at the
// daemon's Unix control socket.
func controlLocalClient(sockPath string) *local.Client {
	return &local.Client{
		Socket:        sockPath,
		UseSocketOnly: true,
		OmitAuth:      true,
	}
}

// controlHTTPClient returns an HTTP client that dials sockPath over AF_UNIX.
// Requests should use controlBaseURL ("http://ts-unplug-proxy/...").
func controlHTTPClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
}

// fetchControlStatus fetches and decodes GET /status from the daemon's Unix
// control socket. Both HTTP 200 (ready) and HTTP 503 (starting / NeedsLogin)
// return a decoded statusDocument without error.
func fetchControlStatus(ctx context.Context, sockPath string) (*statusDocument, error) {
	client := controlHTTPClient(sockPath)
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, controlBaseURL+"/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to control socket %s (is ts-unplug-proxy running?): %w", sockPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("control /status returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var doc statusDocument
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode control /status: %w", err)
	}
	return &doc, nil
}
