package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tailscale.com/atomicfile"
)

// stateFileName is the tsnet state file inside -dir. tsnet's FileStore keeps a
// JSON object of StateKey -> []byte, and Go marshals []byte as base64, so
// map[string][]byte round-trips the file exactly.
const stateFileName = "tailscaled.state"

// persistedPrefs is the subset of ipn.Prefs we inspect offline. The JSON field
// names come from ipn.Prefs; AutoExitNode is an ExitNodeExpression string
// ("any" for automatic exit-node selection) and is omitted when empty.
type persistedPrefs struct {
	Hostname               string   `json:"Hostname"`
	AdvertiseTags          []string `json:"AdvertiseTags"`
	AutoExitNode           string   `json:"AutoExitNode"`
	ExitNodeID             string   `json:"ExitNodeID"`
	ExitNodeIP             string   `json:"ExitNodeIP"`
	ExitNodeAllowLANAccess bool     `json:"ExitNodeAllowLANAccess"`
	InternalExitNodePrior  string   `json:"InternalExitNodePrior"`
	RouteAll               bool     `json:"RouteAll"`
	AdvertiseRoutes        []string `json:"AdvertiseRoutes"`
	CorpDNS                bool     `json:"CorpDNS"`
	WantRunning            bool     `json:"WantRunning"`
}

// stateFile is an in-memory view of the tsnet state file. The file has no
// cross-process lock, so callers must not write it while another tsnet process
// (e.g. a running ts-unplug-proxy) is using the same -dir.
type stateFile struct {
	path string
	data map[string][]byte
}

func stateFilePath(dir string) string { return filepath.Join(dir, stateFileName) }

func loadStateFile(dir string) (*stateFile, error) {
	path := stateFilePath(dir)
	bs, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data := map[string][]byte{}
	if len(bs) > 0 {
		if err := json.Unmarshal(bs, &data); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	return &stateFile{path: path, data: data}, nil
}

// currentProfileKey is the StateKey holding the active profile's prefs
// (e.g. "profile-fc8f").
func (s *stateFile) currentProfileKey() string {
	return string(s.data["_current-profile"])
}

func (s *stateFile) prefs() (*persistedPrefs, error) {
	key := s.currentProfileKey()
	if key == "" {
		return nil, fmt.Errorf("no _current-profile in %s", s.path)
	}
	raw, ok := s.data[key]
	if !ok {
		return nil, fmt.Errorf("no profile %q in %s", key, s.path)
	}
	var p persistedPrefs
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse prefs %q: %w", key, err)
	}
	return &p, nil
}

// magicDNSName returns the tailnet MagicDNS suffix recorded in the state file
// (e.g. "wild-dojo.ts.net"), or "" if it cannot be determined.
func (s *stateFile) magicDNSName() string {
	raw, ok := s.data["_profiles"]
	if !ok {
		return ""
	}
	var profiles map[string]struct {
		NetworkProfile struct {
			MagicDNSName string `json:"MagicDNSName"`
		} `json:"NetworkProfile"`
	}
	if err := json.Unmarshal(raw, &profiles); err != nil {
		return ""
	}
	name := func(p struct {
		NetworkProfile struct {
			MagicDNSName string `json:"MagicDNSName"`
		} `json:"NetworkProfile"`
	}) string {
		return strings.ToLower(strings.TrimSuffix(p.NetworkProfile.MagicDNSName, "."))
	}
	// Prefer the active profile: _current-profile is the state key
	// "profile-<id>", and _profiles is keyed by <id>.
	cur := strings.TrimPrefix(s.currentProfileKey(), "profile-")
	if p, ok := profiles[cur]; ok {
		if n := name(p); n != "" {
			return n
		}
	}
	// Otherwise fall back to a unique name across profiles.
	seen := map[string]bool{}
	var names []string
	for _, p := range profiles {
		n := name(p)
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	if len(names) == 1 {
		return names[0]
	}
	return ""
}

// rawPrefs returns the current profile key and its raw prefs bytes.
func (s *stateFile) rawPrefs() (key string, raw []byte, err error) {
	key = s.currentProfileKey()
	if key == "" {
		return "", nil, fmt.Errorf("no _current-profile in %s", s.path)
	}
	raw, ok := s.data[key]
	if !ok {
		return "", nil, fmt.Errorf("no profile %q in %s", key, s.path)
	}
	return key, raw, nil
}

// setRawPrefs replaces the prefs bytes for key and saves the state file.
func (s *stateFile) setRawPrefs(key string, raw []byte) error {
	s.data[key] = raw
	return s.save()
}

// clearExitNode removes a persisted exit-node preference and returns the names
// of the fields it changed. A no-op returns an empty slice and nil error.
func (s *stateFile) clearExitNode() ([]string, error) {
	key := s.currentProfileKey()
	if key == "" {
		return nil, fmt.Errorf("no _current-profile in %s", s.path)
	}
	raw, ok := s.data[key]
	if !ok {
		return nil, fmt.Errorf("no profile %q in %s", key, s.path)
	}
	// Decode into a generic map so unknown fields survive a rewrite.
	var prefs map[string]any
	if err := json.Unmarshal(raw, &prefs); err != nil {
		return nil, fmt.Errorf("parse prefs %q: %w", key, err)
	}
	var changed []string
	clear := func(field string) {
		v, ok := prefs[field]
		if !ok {
			return
		}
		if s, ok := v.(string); ok && s == "" {
			return
		}
		prefs[field] = ""
		changed = append(changed, field)
	}
	clear("AutoExitNode")
	clear("ExitNodeID")
	clear("ExitNodeIP")
	clear("InternalExitNodePrior")
	if len(changed) == 0 {
		return nil, nil
	}
	updated, err := json.Marshal(prefs)
	if err != nil {
		return nil, err
	}
	s.data[key] = updated
	if err := s.save(); err != nil {
		return nil, err
	}
	return changed, nil
}

// save writes the state file atomically (temp file, fsync, rename), matching
// tsnet's 0600 permissions.
func (s *stateFile) save() error {
	bs, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(s.path, bs, 0o600)
}
