package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTestState(t *testing.T, dir string, prefs map[string]any) {
	t.Helper()
	prefsBytes, err := json.Marshal(prefs)
	if err != nil {
		t.Fatal(err)
	}
	profiles := map[string]any{
		"fc8f": map[string]any{
			"ID":             "fc8f",
			"NetworkProfile": map[string]any{"MagicDNSName": "wild-dojo.ts.net"},
		},
	}
	profilesBytes, err := json.Marshal(profiles)
	if err != nil {
		t.Fatal(err)
	}
	// tsnet's FileStore is a JSON object of StateKey -> []byte (base64).
	state := map[string][]byte{
		"_current-profile": []byte("profile-fc8f"),
		"_profiles":        profilesBytes,
		"profile-fc8f":     prefsBytes,
	}
	bs, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bs, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStateFilePrefsAndMagicDNS(t *testing.T) {
	dir := t.TempDir()
	writeTestState(t, dir, map[string]any{
		"AutoExitNode": "any",
		"ExitNodeID":   "n123",
		"ExitNodeIP":   "100.64.0.5",
		"RouteAll":     true,
		"CorpDNS":      true,
		"WantRunning":  true,
		"UnknownField": "keep-me",
	})

	sf, err := loadStateFile(dir)
	if err != nil {
		t.Fatalf("loadStateFile: %v", err)
	}
	if got := sf.magicDNSName(); got != "wild-dojo.ts.net" {
		t.Errorf("magicDNSName = %q, want wild-dojo.ts.net", got)
	}
	p, err := sf.prefs()
	if err != nil {
		t.Fatalf("prefs: %v", err)
	}
	if p.AutoExitNode != "any" || p.ExitNodeID != "n123" || p.ExitNodeIP != "100.64.0.5" || !p.RouteAll {
		t.Errorf("prefs parsed wrong: %+v", p)
	}
}

func TestMagicDNSPrefersCurrentProfile(t *testing.T) {
	dir := t.TempDir()
	prefsBytes, _ := json.Marshal(map[string]any{"RouteAll": true})
	profiles := map[string]any{
		"aaaa": map[string]any{"NetworkProfile": map[string]any{"MagicDNSName": "work.example.ts.net"}},
		"bbbb": map[string]any{"NetworkProfile": map[string]any{"MagicDNSName": "wild-dojo.ts.net"}},
	}
	profilesBytes, _ := json.Marshal(profiles)
	state := map[string][]byte{
		"_current-profile": []byte("profile-bbbb"),
		"_profiles":        profilesBytes,
		"profile-bbbb":     prefsBytes,
	}
	bs, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bs, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := loadStateFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := sf.magicDNSName(); got != "wild-dojo.ts.net" {
		t.Errorf("magicDNSName = %q, want wild-dojo.ts.net (current profile)", got)
	}
}

func TestClearExitNode(t *testing.T) {
	dir := t.TempDir()
	writeTestState(t, dir, map[string]any{
		"AutoExitNode": "any",
		"ExitNodeID":   "n123",
		"ExitNodeIP":   "100.64.0.5",
		"RouteAll":     true,
		"UnknownField": "keep-me",
	})

	sf, err := loadStateFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := sf.clearExitNode()
	if err != nil {
		t.Fatalf("clearExitNode: %v", err)
	}
	if len(changed) != 3 {
		t.Errorf("changed = %v, want 3 fields", changed)
	}

	// Reload from disk to prove persistence.
	sf2, err := loadStateFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := sf2.prefs()
	if err != nil {
		t.Fatal(err)
	}
	if p.AutoExitNode != "" || p.ExitNodeID != "" || p.ExitNodeIP != "" {
		t.Errorf("exit-node prefs not cleared: %+v", p)
	}
	if !p.RouteAll {
		t.Error("RouteAll was cleared, want preserved")
	}

	// Unknown fields must survive the rewrite.
	raw := sf2.data[sf2.currentProfileKey()]
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["UnknownField"] != "keep-me" {
		t.Errorf("UnknownField = %v, want keep-me", m["UnknownField"])
	}

	// A second clear is a no-op.
	changed, err = sf2.clearExitNode()
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Errorf("second clear changed %v, want none", changed)
	}
}
