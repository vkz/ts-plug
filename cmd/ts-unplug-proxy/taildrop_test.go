package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

type mockUploadedFile struct {
	TargetID      string
	Name          string
	Body          []byte
	ContentLength int64
}

type mockTaildropServer struct {
	sockPath string

	mu       sync.Mutex
	targets  []apitype.FileTarget
	waiting  []apitype.WaitingFile
	contents map[string][]byte

	uploads []mockUploadedFile
	gets    []string
	deletes []string
}

func startMockTaildropServer(t *testing.T, targets []apitype.FileTarget, files map[string][]byte) *mockTaildropServer {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket tests require Unix")
	}

	dir, err := os.MkdirTemp("/tmp", "tsup-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen unix %s: %v", sockPath, err)
	}

	m := &mockTaildropServer{
		sockPath: sockPath,
		targets:  targets,
		contents: make(map[string][]byte, len(files)),
	}
	for name, data := range files {
		cp := append([]byte(nil), data...)
		m.contents[name] = cp
		m.waiting = append(m.waiting, apitype.WaitingFile{
			Name: name,
			Size: int64(len(cp)),
		})
	}

	srv := &http.Server{Handler: http.HandlerFunc(m.serveHTTP)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})

	return m
}

func (m *mockTaildropServer) setWaiting(waiting []apitype.WaitingFile, contents map[string][]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.waiting = append([]apitype.WaitingFile(nil), waiting...)
	m.contents = make(map[string][]byte, len(contents))
	for k, v := range contents {
		m.contents[k] = append([]byte(nil), v...)
	}
}

func (m *mockTaildropServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/localapi/v0/file-targets":
		m.mu.Lock()
		targets := append([]apitype.FileTarget(nil), m.targets...)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(targets)

	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/localapi/v0/file-put/"):
		// Use EscapedPath so url.PathEscape encoding is decoded cleanly.
		rawRest := strings.TrimPrefix(r.URL.EscapedPath(), "/localapi/v0/file-put/")
		targetID, escapedName, ok := strings.Cut(rawRest, "/")
		if !ok || targetID == "" || escapedName == "" {
			http.Error(w, "invalid file-put path", http.StatusBadRequest)
			return
		}
		name, err := url.PathUnescape(escapedName)
		if err != nil {
			http.Error(w, "invalid escaped filename", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m.mu.Lock()
		m.uploads = append(m.uploads, mockUploadedFile{
			TargetID:      targetID,
			Name:          name,
			Body:          body,
			ContentLength: r.ContentLength,
		})
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodGet && r.URL.Path == "/localapi/v0/files/":
		m.mu.Lock()
		waiting := append([]apitype.WaitingFile(nil), m.waiting...)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(waiting)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/localapi/v0/files/"):
		rawName := strings.TrimPrefix(r.URL.EscapedPath(), "/localapi/v0/files/")
		name, err := url.PathUnescape(rawName)
		if err != nil {
			http.Error(w, "invalid name", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.gets = append(m.gets, name)
		data, ok := m.contents[name]
		m.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/localapi/v0/files/"):
		rawName := strings.TrimPrefix(r.URL.EscapedPath(), "/localapi/v0/files/")
		name, err := url.PathUnescape(rawName)
		if err != nil {
			http.Error(w, "invalid name", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.deletes = append(m.deletes, name)
		delete(m.contents, name)
		filtered := m.waiting[:0]
		for _, wf := range m.waiting {
			if wf.Name != name {
				filtered = append(filtered, wf)
			}
		}
		m.waiting = filtered
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	default:
		http.NotFound(w, r)
	}
}

func sampleFileTargets() []apitype.FileTarget {
	return []apitype.FileTarget{
		{
			Node: &tailcfg.Node{
				StableID:     "node-laptop",
				Name:         "laptop.wild-dojo.ts.net.",
				ComputedName: "laptop",
				Hostinfo:     (&tailcfg.Hostinfo{Hostname: "MacBook-Pro"}).View(),
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.0.10/32"),
					netip.MustParsePrefix("fd7a:115c:a1e0::10/128"),
				},
			},
			PeerAPIURL: "http://100.64.0.10:12345",
		},
		{
			Node: &tailcfg.Node{
				StableID:     "node-server",
				Name:         "gxa.wild-dojo.ts.net.",
				ComputedName: "gxa",
				Hostinfo:     (&tailcfg.Hostinfo{Hostname: "gxa-box"}).View(),
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.0.20/32"),
				},
			},
			PeerAPIURL: "http://100.64.0.20:12345",
		},
	}
}

func TestSanitizeTaildropName(t *testing.T) {
	valid := []string{
		"report.pdf",
		"hello world (1).txt",
		".hidden",
		"archive.tar.gz",
		"a",
		"file-with-dashes_and_underscores.bin",
	}
	for _, name := range valid {
		got, err := sanitizeTaildropName(name)
		if err != nil {
			t.Errorf("sanitizeTaildropName(%q) unexpected error: %v", name, err)
		}
		if got != name {
			t.Errorf("sanitizeTaildropName(%q) = %q, want %q", name, got, name)
		}
	}

	invalid := []string{
		"",
		".",
		"..",
		"../evil.txt",
		"..\\evil.txt",
		"a/b.txt",
		"a\\b.txt",
		"/etc/passwd",
		"foo/bar/baz",
		"evil\x00.txt",
		"sub/..",
		"./file.txt",
	}
	for _, name := range invalid {
		if _, err := sanitizeTaildropName(name); err == nil {
			t.Errorf("sanitizeTaildropName(%q) succeeded, want error", name)
		}
	}
}

func TestTaildropHelpOutputs(t *testing.T) {
	cases := []struct {
		name     string
		run      func(stdout, stderr *bytes.Buffer) int
		contains []string
	}{
		{
			name: "file -h",
			run: func(out, err *bytes.Buffer) int {
				return runFileIO([]string{"-h"}, strings.NewReader(""), out, err)
			},
			contains: []string{"cp", "get", "ls", "targets", "SOCKET RESOLUTION", "-advertise-tags"},
		},
		{
			name: "file --help",
			run: func(out, err *bytes.Buffer) int {
				return runFileIO([]string{"--help"}, strings.NewReader(""), out, err)
			},
			contains: []string{"SUBCOMMANDS", "EXAMPLES", "SOCKET RESOLUTION", "-advertise-tags"},
		},
		{
			name: "file cp -h",
			run: func(out, err *bytes.Buffer) int {
				return runFileIO([]string{"cp", "-h"}, strings.NewReader(""), out, err)
			},
			contains: []string{"-name", "-socket", "-dir", "EXAMPLES", "SOCKET RESOLUTION", "-advertise-tags"},
		},
		{
			name: "file cp --help",
			run: func(out, err *bytes.Buffer) int {
				return runFileIO([]string{"cp", "--help"}, strings.NewReader(""), out, err)
			},
			contains: []string{"-name", "trailing colon", "SOCKET RESOLUTION", "-advertise-tags"},
		},
		{
			name: "file get -h",
			run: func(out, err *bytes.Buffer) int {
				return runFileIO([]string{"get", "-h"}, strings.NewReader(""), out, err)
			},
			contains: []string{"-conflict", "-keep", "-socket", "-dir", "EXAMPLES", "SOCKET RESOLUTION", "-advertise-tags"},
		},
		{
			name: "file get --help",
			run: func(out, err *bytes.Buffer) int {
				return runFileIO([]string{"get", "--help"}, strings.NewReader(""), out, err)
			},
			contains: []string{"skip", "rename", "overwrite", "SOCKET RESOLUTION", "-advertise-tags"},
		},
		{
			name: "file ls -h",
			run: func(out, err *bytes.Buffer) int {
				return runFileIO([]string{"ls", "-h"}, strings.NewReader(""), out, err)
			},
			contains: []string{"-json", "-socket", "-dir", "EXAMPLES", "SOCKET RESOLUTION", "-advertise-tags"},
		},
		{
			name: "file ls --help",
			run: func(out, err *bytes.Buffer) int {
				return runFileIO([]string{"ls", "--help"}, strings.NewReader(""), out, err)
			},
			contains: []string{"-json", "EXAMPLES", "SOCKET RESOLUTION", "-advertise-tags"},
		},
		{
			name: "file targets -h",
			run: func(out, err *bytes.Buffer) int {
				return runFileIO([]string{"targets", "-h"}, strings.NewReader(""), out, err)
			},
			contains: []string{"-json", "-socket", "-dir", "EXAMPLES", "SOCKET RESOLUTION", "-advertise-tags"},
		},
		{
			name: "file targets --help",
			run: func(out, err *bytes.Buffer) int {
				return runFileIO([]string{"targets", "--help"}, strings.NewReader(""), out, err)
			},
			contains: []string{"-json", "EXAMPLES", "SOCKET RESOLUTION", "-advertise-tags"},
		},
		{
			name: "cp -h",
			run: func(out, err *bytes.Buffer) int {
				return runCPIO([]string{"-h"}, strings.NewReader(""), out, err)
			},
			contains: []string{"file cp", "file get", "file ls", "file targets", "-name", "SOCKET RESOLUTION", "-advertise-tags"},
		},
		{
			name: "cp --help",
			run: func(out, err *bytes.Buffer) int {
				return runCPIO([]string{"--help"}, strings.NewReader(""), out, err)
			},
			contains: []string{"file get", "file ls", "file targets", "EXAMPLES", "SOCKET RESOLUTION", "-advertise-tags"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := tc.run(&stdout, &stderr)
			if code != 0 {
				t.Errorf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout not empty on help: %q", stdout.String())
			}
			gotErr := stderr.String()
			for _, sub := range tc.contains {
				if !strings.Contains(gotErr, sub) {
					t.Errorf("help text missing %q:\n%s", sub, gotErr)
				}
			}
		})
	}
}

func TestFileUnknownAndMissingSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runFileIO(nil, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("file with no args exit = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout not empty: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runFileIO([]string{"bogus"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("file bogus exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr = %q, want unknown subcommand error", stderr.String())
	}
}

func TestFileTargetsCommand(t *testing.T) {
	m := startMockTaildropServer(t, sampleFileTargets(), nil)

	// Human table output.
	var stdout, stderr bytes.Buffer
	code := runFileIO([]string{"targets", "-socket", m.sockPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("file targets exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"NAME", "DNS", "IP", "laptop", "laptop.wild-dojo.ts.net", "100.64.0.10", "gxa", "100.64.0.20"} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q:\n%s", want, out)
		}
	}

	// JSON output.
	stdout.Reset()
	stderr.Reset()
	code = runFileIO([]string{"targets", "-json", "-socket", m.sockPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("file targets -json exit = %d, stderr = %s", code, stderr.String())
	}
	var decoded []apitype.FileTarget
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode JSON: %v\nraw:\n%s", err, stdout.String())
	}
	if len(decoded) != 2 || decoded[0].Node.StableID != "node-laptop" || decoded[1].Node.StableID != "node-server" {
		t.Fatalf("decoded targets = %+v", decoded)
	}

	// Empty targets (human + JSON).
	mEmpty := startMockTaildropServer(t, nil, nil)
	stdout.Reset()
	stderr.Reset()
	code = runFileIO([]string{"targets", "-socket", mEmpty.sockPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("empty file targets exit = %d, stderr = %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "No Taildrop targets available." {
		t.Errorf("empty output = %q, want 'No Taildrop targets available.'", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = runFileIO([]string{"targets", "-json", "-socket", mEmpty.sockPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("empty file targets -json exit = %d", code)
	}
	if strings.TrimSpace(stdout.String()) != "[]" {
		t.Errorf("empty json = %q, want []", stdout.String())
	}
}

func TestFileLSCommand(t *testing.T) {
	m := startMockTaildropServer(t, nil, nil)
	m.setWaiting(
		[]apitype.WaitingFile{
			{Name: "notes.txt", Size: 42},
			{Name: "archive.tar.gz", Size: 2048},
		},
		map[string][]byte{
			"notes.txt":      bytes.Repeat([]byte("a"), 42),
			"archive.tar.gz": bytes.Repeat([]byte("b"), 2048),
		},
	)

	// Human table output.
	var stdout, stderr bytes.Buffer
	code := runFileIO([]string{"ls", "-socket", m.sockPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("file ls exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"NAME", "SIZE", "notes.txt", "42 B", "archive.tar.gz", "2.0 KiB"} {
		if !strings.Contains(out, want) {
			t.Errorf("file ls output missing %q:\n%s", want, out)
		}
	}

	// JSON output.
	stdout.Reset()
	stderr.Reset()
	code = runFileIO([]string{"ls", "-json", "-socket", m.sockPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("file ls -json exit = %d, stderr = %s", code, stderr.String())
	}
	var decoded []apitype.WaitingFile
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode JSON: %v\nraw:\n%s", err, stdout.String())
	}
	if len(decoded) != 2 || decoded[0].Name != "notes.txt" || decoded[0].Size != 42 {
		t.Fatalf("decoded waiting files = %+v", decoded)
	}

	// Empty waiting list (human + JSON).
	mEmpty := startMockTaildropServer(t, nil, nil)
	stdout.Reset()
	stderr.Reset()
	code = runFileIO([]string{"ls", "-socket", mEmpty.sockPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("empty file ls exit = %d", code)
	}
	if strings.TrimSpace(stdout.String()) != "No waiting files." {
		t.Errorf("empty file ls output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = runFileIO([]string{"ls", "-json", "-socket", mEmpty.sockPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("empty file ls -json exit = %d", code)
	}
	if strings.TrimSpace(stdout.String()) != "[]" {
		t.Errorf("empty file ls -json = %q, want []", stdout.String())
	}
}

func TestFileCPAndShorthand(t *testing.T) {
	m := startMockTaildropServer(t, sampleFileTargets(), nil)

	tmpDir := t.TempDir()
	fileA := filepath.Join(tmpDir, "hello.txt")
	fileB := filepath.Join(tmpDir, "data.bin")
	payloadA := []byte("hello taildrop world\n")
	payloadB := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}
	if err := os.WriteFile(fileA, payloadA, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, payloadB, 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Send by short name (case-insensitive) using 'file cp'.
	var stdout, stderr bytes.Buffer
	code := runFileIO([]string{"cp", "-socket", m.sockPath, fileA, "LAPTOP:"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("file cp short name exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Sent hello.txt") {
		t.Errorf("stderr = %q, want Sent message", stderr.String())
	}

	// 2. Send by FQDN with trailing dot using 'cp' shorthand + '-name' override.
	stdout.Reset()
	stderr.Reset()
	code = runCPIO([]string{"-socket", m.sockPath, "-name", "renamed.txt", fileA, "laptop.wild-dojo.ts.net.:"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cp shorthand FQDN exit = %d, stderr = %s", code, stderr.String())
	}

	// 3. Send multiple files by Tailscale IPv4.
	stdout.Reset()
	stderr.Reset()
	code = runFileIO([]string{"cp", "-socket", m.sockPath, fileA, fileB, "100.64.0.20:"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("file cp multi by IP exit = %d, stderr = %s", code, stderr.String())
	}

	// 4. Send by Hostinfo hostname and IPv6 with brackets.
	stdout.Reset()
	stderr.Reset()
	code = runCPIO([]string{"-socket", m.sockPath, fileB, "macbook-pro:"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cp by Hostinfo hostname exit = %d, stderr = %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = runFileIO([]string{"cp", "-socket", m.sockPath, fileA, "[fd7a:115c:a1e0::10]:"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("file cp by IPv6 exit = %d, stderr = %s", code, stderr.String())
	}

	// 5. Send from stdin '-' with '-name'.
	stdinPayload := "streamed from stdin!"
	stdout.Reset()
	stderr.Reset()
	code = runFileIO([]string{"cp", "-socket", m.sockPath, "-name", "stdin-note.txt", "-", "gxa:"}, strings.NewReader(stdinPayload), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("file cp stdin exit = %d, stderr = %s", code, stderr.String())
	}

	// Verify all uploaded files and exact bytes.
	m.mu.Lock()
	uploads := append([]mockUploadedFile(nil), m.uploads...)
	m.mu.Unlock()

	if len(uploads) != 7 {
		t.Fatalf("upload count = %d, want 7", len(uploads))
	}
	checkUpload := func(idx int, wantTarget, wantName string, wantBody []byte) {
		t.Helper()
		u := uploads[idx]
		if u.TargetID != wantTarget || u.Name != wantName || !bytes.Equal(u.Body, wantBody) {
			t.Errorf("upload[%d] = {target:%q name:%q body:%q}, want {target:%q name:%q body:%q}",
				idx, u.TargetID, u.Name, string(u.Body), wantTarget, wantName, string(wantBody))
		}
	}
	checkUpload(0, "node-laptop", "hello.txt", payloadA)
	checkUpload(1, "node-laptop", "renamed.txt", payloadA)
	checkUpload(2, "node-server", "hello.txt", payloadA)
	checkUpload(3, "node-server", "data.bin", payloadB)
	checkUpload(4, "node-laptop", "data.bin", payloadB)
	checkUpload(5, "node-laptop", "hello.txt", payloadA)
	checkUpload(6, "node-server", "stdin-note.txt", []byte(stdinPayload))
	if uploads[0].ContentLength != int64(len(payloadA)) {
		t.Errorf("regular file ContentLength = %d, want %d", uploads[0].ContentLength, len(payloadA))
	}
	if uploads[6].ContentLength != -1 {
		t.Errorf("stdin ContentLength = %d, want -1 (chunked stream)", uploads[6].ContentLength)
	}
}

func TestFileCPErrors(t *testing.T) {
	m := startMockTaildropServer(t, sampleFileTargets(), nil)

	tmpDir := t.TempDir()
	fileA := filepath.Join(tmpDir, "a.txt")
	if err := os.WriteFile(fileA, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(tmpDir, "adir")
	if err := os.Mkdir(subDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Missing trailing ':' -> exit 2.
	var stdout, stderr bytes.Buffer
	code := runFileIO([]string{"cp", "-socket", m.sockPath, fileA, "laptop"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Errorf("missing colon exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "destination must end with ':'") {
		t.Errorf("stderr = %q, want missing colon message", stderr.String())
	}

	// Stdin without -name -> exit 2.
	stderr.Reset()
	code = runFileIO([]string{"cp", "-socket", m.sockPath, "-", "laptop:"}, strings.NewReader("hi"), &stdout, &stderr)
	if code != 2 {
		t.Errorf("stdin without -name exit = %d, want 2", code)
	}

	// -name with multiple files -> exit 2.
	stderr.Reset()
	code = runFileIO([]string{"cp", "-socket", m.sockPath, "-name", "x.txt", fileA, fileA, "laptop:"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Errorf("-name with multiple files exit = %d, want 2", code)
	}

	// Unsafe -name -> exit 2.
	stderr.Reset()
	code = runFileIO([]string{"cp", "-socket", m.sockPath, "-name", "../evil.txt", fileA, "laptop:"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Errorf("unsafe -name exit = %d, want 2", code)
	}

	// Unknown peer -> exit 1 and lists available targets.
	stderr.Reset()
	code = runFileIO([]string{"cp", "-socket", m.sockPath, fileA, "nonexistent:"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("unknown peer exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "laptop") || !strings.Contains(stderr.String(), "gxa") {
		t.Errorf("unknown peer stderr = %q, want available target names listed", stderr.String())
	}

	// Directory rejected cleanly (no upload performed), even when preceded by a valid file.
	goodFile := filepath.Join(filepath.Dir(subDir), "good.txt")
	if err := os.WriteFile(goodFile, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	code = runFileIO([]string{"cp", "-socket", m.sockPath, goodFile, subDir, "laptop:"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("directory exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "directories are not supported") {
		t.Errorf("directory stderr = %q", stderr.String())
	}
	m.mu.Lock()
	uploadCount := len(m.uploads)
	m.mu.Unlock()
	if uploadCount != 0 {
		t.Errorf("expected 0 uploads after error cases, got %d", uploadCount)
	}

	// Empty targets -> actionable error mentioning ACL / untagged nodes.
	mEmpty := startMockTaildropServer(t, nil, nil)
	stderr.Reset()
	code = runFileIO([]string{"cp", "-socket", mEmpty.sockPath, fileA, "laptop:"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("empty targets exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "ACL") || !strings.Contains(stderr.String(), "untagged") {
		t.Errorf("empty targets stderr = %q, want actionable ACL/untagged guidance", stderr.String())
	}

	// Ambiguous peer -> exit 1 listing both matching peers.
	ambTargets := []apitype.FileTarget{
		{
			Node: &tailcfg.Node{
				StableID:     "node-1",
				Name:         "dev-1.wild-dojo.ts.net.",
				ComputedName: "dev-1",
				Hostinfo:     (&tailcfg.Hostinfo{Hostname: "dev"}).View(),
				Addresses:    []netip.Prefix{netip.MustParsePrefix("100.64.0.1/32")},
			},
		},
		{
			Node: &tailcfg.Node{
				StableID:     "node-2",
				Name:         "dev-2.wild-dojo.ts.net.",
				ComputedName: "dev-2",
				Hostinfo:     (&tailcfg.Hostinfo{Hostname: "dev"}).View(),
				Addresses:    []netip.Prefix{netip.MustParsePrefix("100.64.0.2/32")},
			},
		},
	}
	mAmb := startMockTaildropServer(t, ambTargets, nil)
	stderr.Reset()
	code = runFileIO([]string{"cp", "-socket", mAmb.sockPath, fileA, "dev:"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("ambiguous peer exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "ambiguous") || !strings.Contains(stderr.String(), "dev-1") || !strings.Contains(stderr.String(), "dev-2") {
		t.Errorf("ambiguous stderr = %q, want ambiguous listing dev-1 and dev-2", stderr.String())
	}
}

func TestFileGetNormalAndKeep(t *testing.T) {
	// 1. Normal download verifies contents and DeleteWaitingFile called.
	payload := []byte("received payload 12345")
	m := startMockTaildropServer(t, nil, map[string][]byte{
		"doc.txt": payload,
	})
	destDir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := runFileIO([]string{"get", "-socket", m.sockPath, destDir}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("file get exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Received doc.txt") {
		t.Errorf("stdout = %q, want Received doc.txt", stdout.String())
	}
	gotBytes, err := os.ReadFile(filepath.Join(destDir, "doc.txt"))
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if !bytes.Equal(gotBytes, payload) {
		t.Errorf("downloaded bytes = %q, want %q", gotBytes, payload)
	}
	m.mu.Lock()
	deletes := append([]string(nil), m.deletes...)
	m.mu.Unlock()
	if len(deletes) != 1 || deletes[0] != "doc.txt" {
		t.Errorf("deletes = %v, want [doc.txt]", deletes)
	}
	// Ensure no leftover temp files.
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "doc.txt" {
		t.Errorf("destDir entries = %v, want only doc.txt", entries)
	}

	// 2. Running again when daemon has no waiting files prints "No waiting files.".
	stdout.Reset()
	stderr.Reset()
	code = runFileIO([]string{"get", "-socket", m.sockPath, destDir}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("empty file get exit = %d, stderr = %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "No waiting files." {
		t.Errorf("empty get stdout = %q", stdout.String())
	}

	// 3. -keep downloads file and does NOT delete from daemon.
	mKeep := startMockTaildropServer(t, nil, map[string][]byte{
		"keepme.txt": []byte("stay on daemon"),
	})
	destKeep := t.TempDir()
	stdout.Reset()
	stderr.Reset()
	code = runFileIO([]string{"get", "-keep", "-socket", mKeep.sockPath, destKeep}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("file get -keep exit = %d, stderr = %s", code, stderr.String())
	}
	gotKeep, err := os.ReadFile(filepath.Join(destKeep, "keepme.txt"))
	if err != nil || string(gotKeep) != "stay on daemon" {
		t.Fatalf("keepme.txt = %q, err = %v", gotKeep, err)
	}
	mKeep.mu.Lock()
	keepDeletes := append([]string(nil), mKeep.deletes...)
	mKeep.mu.Unlock()
	if len(keepDeletes) != 0 {
		t.Errorf("-keep triggered deletes: %v", keepDeletes)
	}
}

func TestFileGetConflictPolicies(t *testing.T) {
	// 1. -conflict=skip leaves existing file untouched AND does NOT delete from daemon.
	t.Run("skip", func(t *testing.T) {
		m := startMockTaildropServer(t, nil, map[string][]byte{
			"foo.txt": []byte("new-from-daemon"),
		})
		destDir := t.TempDir()
		existingPath := filepath.Join(destDir, "foo.txt")
		if err := os.WriteFile(existingPath, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		code := runFileIO([]string{"get", "-conflict", "skip", "-socket", m.sockPath, destDir}, strings.NewReader(""), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("skip exit = %d, stderr = %s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "Skipping foo.txt (already exists)") {
			t.Errorf("stderr = %q, want Skipping message", stderr.String())
		}
		got, err := os.ReadFile(existingPath)
		if err != nil || string(got) != "original" {
			t.Errorf("existing file modified: %q err=%v", got, err)
		}
		m.mu.Lock()
		deletes := append([]string(nil), m.deletes...)
		gets := append([]string(nil), m.gets...)
		m.mu.Unlock()
		if len(deletes) != 0 {
			t.Errorf("skip deleted from daemon: %v", deletes)
		}
		if len(gets) != 0 {
			t.Errorf("skip fast-path unexpectedly downloaded file: %v", gets)
		}
	})

	// 2. -conflict=rename writes foo-1.txt (and preserves existing foo.txt) AND deletes from daemon.
	t.Run("rename", func(t *testing.T) {
		m := startMockTaildropServer(t, nil, map[string][]byte{
			"foo.txt": []byte("incoming-v2"),
		})
		destDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(destDir, "foo.txt"), []byte("original-v1"), 0o600); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		code := runFileIO([]string{"get", "-conflict", "rename", "-socket", m.sockPath, destDir}, strings.NewReader(""), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("rename exit = %d, stderr = %s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Received foo-1.txt") {
			t.Errorf("stdout = %q, want Received foo-1.txt", stdout.String())
		}
		orig, err := os.ReadFile(filepath.Join(destDir, "foo.txt"))
		if err != nil || string(orig) != "original-v1" {
			t.Errorf("original foo.txt = %q err=%v", orig, err)
		}
		renamed, err := os.ReadFile(filepath.Join(destDir, "foo-1.txt"))
		if err != nil || string(renamed) != "incoming-v2" {
			t.Errorf("foo-1.txt = %q err=%v", renamed, err)
		}
		m.mu.Lock()
		deletes := append([]string(nil), m.deletes...)
		m.mu.Unlock()
		if len(deletes) != 1 || deletes[0] != "foo.txt" {
			t.Errorf("rename deletes = %v, want [foo.txt]", deletes)
		}
	})

	// 3. -conflict=overwrite atomically replaces existing file AND deletes from daemon.
	t.Run("overwrite", func(t *testing.T) {
		m := startMockTaildropServer(t, nil, map[string][]byte{
			"foo.txt": []byte("replaced-content"),
		})
		destDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(destDir, "foo.txt"), []byte("old-content"), 0o600); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		code := runFileIO([]string{"get", "-conflict", "overwrite", "-socket", m.sockPath, destDir}, strings.NewReader(""), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("overwrite exit = %d, stderr = %s", code, stderr.String())
		}
		got, err := os.ReadFile(filepath.Join(destDir, "foo.txt"))
		if err != nil || string(got) != "replaced-content" {
			t.Errorf("foo.txt = %q err=%v, want replaced-content", got, err)
		}
		m.mu.Lock()
		deletes := append([]string(nil), m.deletes...)
		m.mu.Unlock()
		if len(deletes) != 1 || deletes[0] != "foo.txt" {
			t.Errorf("overwrite deletes = %v, want [foo.txt]", deletes)
		}
	})

	// 4. Invalid -conflict value -> exit 2.
	t.Run("invalid-conflict", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runFileIO([]string{"get", "-conflict", "clobber"}, strings.NewReader(""), &stdout, &stderr)
		if code != 2 {
			t.Errorf("invalid conflict exit = %d, want 2", code)
		}
	})
}

func TestFileGetPathTraversalRejected(t *testing.T) {
	parentDir := t.TempDir()
	destDir := filepath.Join(parentDir, "inbox")
	if err := os.Mkdir(destDir, 0o700); err != nil {
		t.Fatal(err)
	}

	maliciousNames := []string{
		"../evil.txt",
		"a/b.txt",
		`..\evil2.txt`,
		"..",
		".",
		"/tmp/abs-evil.txt",
		"null\x00byte.txt",
	}

	waiting := make([]apitype.WaitingFile, 0, len(maliciousNames))
	contents := make(map[string][]byte, len(maliciousNames))
	for _, name := range maliciousNames {
		waiting = append(waiting, apitype.WaitingFile{Name: name, Size: 7})
		contents[name] = []byte("pwned!\n")
	}

	m := startMockTaildropServer(t, nil, nil)
	m.setWaiting(waiting, contents)

	var stdout, stderr bytes.Buffer
	code := runFileIO([]string{"get", "-conflict", "overwrite", "-socket", m.sockPath, destDir}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("path traversal exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "refusing unsafe filename") {
		t.Errorf("stderr = %q, want refusing unsafe filename", stderr.String())
	}

	// Nothing written to parentDir (e.g. evil.txt) or destDir.
	if _, err := os.Stat(filepath.Join(parentDir, "evil.txt")); !os.IsNotExist(err) {
		t.Errorf("evil.txt was written outside destDir!")
	}
	if _, err := os.Stat(filepath.Join(parentDir, "evil2.txt")); !os.IsNotExist(err) {
		t.Errorf("evil2.txt was written outside destDir!")
	}
	destEntries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(destEntries) != 0 {
		t.Errorf("destDir should be empty, found %d entries", len(destEntries))
	}

	// Neither GetWaitingFile nor DeleteWaitingFile should have been called.
	m.mu.Lock()
	gets := append([]string(nil), m.gets...)
	deletes := append([]string(nil), m.deletes...)
	m.mu.Unlock()
	if len(gets) != 0 {
		t.Errorf("server received GetWaitingFile for unsafe names: %v", gets)
	}
	if len(deletes) != 0 {
		t.Errorf("server received DeleteWaitingFile for unsafe names: %v", deletes)
	}
}

func TestFileGetDownloadFailureCleansUpAndDoesNotDelete(t *testing.T) {
	// WaitingFiles lists "broken.txt", but contents map omits it so GET returns 404.
	m := startMockTaildropServer(t, nil, nil)
	m.setWaiting(
		[]apitype.WaitingFile{{Name: "broken.txt", Size: 10}},
		map[string][]byte{},
	)
	destDir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := runFileIO([]string{"get", "-socket", m.sockPath, destDir}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("failed download exit = %d, want 1", code)
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("destDir should have no leftover files, found %v", entries)
	}
	m.mu.Lock()
	deletes := append([]string(nil), m.deletes...)
	m.mu.Unlock()
	if len(deletes) != 0 {
		t.Errorf("failed download triggered DeleteWaitingFile: %v", deletes)
	}
}

func TestFileGetDestDirValidationAndDirFlag(t *testing.T) {
	// Non-existent destination directory fails cleanly.
	var stdout, stderr bytes.Buffer
	code := runFileIO([]string{"get", filepath.Join(t.TempDir(), "does-not-exist")}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("non-existent dest exit = %d, want 1", code)
	}

	// Regular file as destination directory fails cleanly.
	regFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(regFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	code = runFileIO([]string{"get", regFile}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("file dest exit = %d, want 1", code)
	}

	// -dir flag resolves <dir>/control.sock.
	m := startMockTaildropServer(t, sampleFileTargets(), nil)
	sockDir := filepath.Dir(m.sockPath)
	stdout.Reset()
	stderr.Reset()
	code = runFileIO([]string{"targets", "-dir", sockDir}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("targets -dir exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "laptop") {
		t.Errorf("targets -dir output = %q, want laptop", stdout.String())
	}
}

func TestRenameCandidate(t *testing.T) {
	cases := []struct {
		name    string
		attempt int
		want    string
	}{
		{"foo.txt", 0, "foo.txt"},
		{"foo.txt", 1, "foo-1.txt"},
		{"foo.txt", 2, "foo-2.txt"},
		{"README", 1, "README-1"},
		{".env", 1, ".env-1"},
		{"archive.tar.gz", 1, "archive.tar-1.gz"},
	}
	for _, tc := range cases {
		if got := renameCandidate(tc.name, tc.attempt); got != tc.want {
			t.Errorf("renameCandidate(%q, %d) = %q, want %q", tc.name, tc.attempt, got, tc.want)
		}
	}
}
