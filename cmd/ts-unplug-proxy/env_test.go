package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func startMockControlStatusServer(t *testing.T, doc statusDocument, statusCode int) (sockDir, sockPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix control socket tests require Unix")
	}
	dir, err := os.MkdirTemp("/tmp", "tsup-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sockPath = filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_ = json.NewEncoder(w).Encode(doc)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return dir, sockPath
}

func TestEnvHelpAndArgValidation(t *testing.T) {
	for _, helpArg := range []string{"-h", "--help"} {
		var stdout, stderr bytes.Buffer
		code := runEnvIO([]string{helpArg}, nil, &stdout, &stderr, nil)
		if code != 0 {
			t.Errorf("runEnvIO(%q) exit = %d, want 0", helpArg, code)
		}
		if stdout.Len() != 0 {
			t.Errorf("runEnvIO(%q) stdout = %q, want empty", helpArg, stdout.String())
		}
		help := stderr.String()
		for _, want := range []string{"-socks5", "-http", "-no-proxy", "-print", "-dir", "-socket", "-tailnet-only"} {
			if !strings.Contains(help, want) {
				t.Errorf("runEnvIO(%q) help text missing %q", helpArg, want)
			}
		}
	}

	// Missing command without -print -> exit 2.
	for _, args := range [][]string{
		nil,
		{"-socks5", "127.0.0.1:1080"},
		{"-socks5", "127.0.0.1:1080", "--"},
	} {
		var stdout, stderr bytes.Buffer
		code := runEnvIO(args, nil, &stdout, &stderr, nil)
		if code != 2 {
			t.Errorf("runEnvIO(%v) exit = %d, want 2", args, code)
		}
		if stdout.Len() != 0 {
			t.Errorf("runEnvIO(%v) stdout = %q, want empty", args, stdout.String())
		}
		if !strings.Contains(stderr.String(), "missing command") {
			t.Errorf("runEnvIO(%v) stderr = %q, want missing command hint", args, stderr.String())
		}
	}

	// Positional command with -print -> exit 2.
	for _, args := range [][]string{
		{"-print", "curl"},
		{"-print", "--", "curl", "https://example.com"},
	} {
		var stdout, stderr bytes.Buffer
		code := runEnvIO(args, nil, &stdout, &stderr, nil)
		if code != 2 {
			t.Errorf("runEnvIO(%v) exit = %d, want 2", args, code)
		}
		if stdout.Len() != 0 {
			t.Errorf("runEnvIO(%v) stdout = %q, want empty", args, stdout.String())
		}
		if !strings.Contains(stderr.String(), "-print") {
			t.Errorf("runEnvIO(%v) stderr = %q, want -print rejection", args, stderr.String())
		}
	}

	// Unknown flag -> exit 2.
	var stdout, stderr bytes.Buffer
	if code := runEnvIO([]string{"-no-such-flag"}, nil, &stdout, &stderr, nil); code != 2 {
		t.Errorf("unknown flag exit = %d, want 2", code)
	}
}

func TestBuildProxyEnv(t *testing.T) {
	t.Run("socks5 only", func(t *testing.T) {
		got := buildProxyEnv("127.0.0.1:1080", "", "", nil)
		want := map[string]string{
			"ALL_PROXY": "socks5h://127.0.0.1:1080",
			"all_proxy": "socks5h://127.0.0.1:1080",
			"NO_PROXY":  "localhost,127.0.0.1,::1",
			"no_proxy":  "localhost,127.0.0.1,::1",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("buildProxyEnv socks5-only =\n%#v\nwant:\n%#v", got, want)
		}
	})

	t.Run("http only sets ALL_PROXY too", func(t *testing.T) {
		got := buildProxyEnv("", "127.0.0.1:8080", "", nil)
		want := map[string]string{
			"ALL_PROXY":   "http://127.0.0.1:8080",
			"all_proxy":   "http://127.0.0.1:8080",
			"HTTP_PROXY":  "http://127.0.0.1:8080",
			"http_proxy":  "http://127.0.0.1:8080",
			"HTTPS_PROXY": "http://127.0.0.1:8080",
			"https_proxy": "http://127.0.0.1:8080",
			"NO_PROXY":    "localhost,127.0.0.1,::1",
			"no_proxy":    "localhost,127.0.0.1,::1",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("buildProxyEnv http-only =\n%#v\nwant:\n%#v", got, want)
		}
	})

	t.Run("both socks5 and http", func(t *testing.T) {
		got := buildProxyEnv("127.0.0.1:1080", "127.0.0.1:8080", "", nil)
		want := map[string]string{
			"ALL_PROXY":   "socks5h://127.0.0.1:1080",
			"all_proxy":   "socks5h://127.0.0.1:1080",
			"HTTP_PROXY":  "http://127.0.0.1:8080",
			"http_proxy":  "http://127.0.0.1:8080",
			"HTTPS_PROXY": "http://127.0.0.1:8080",
			"https_proxy": "http://127.0.0.1:8080",
			"NO_PROXY":    "localhost,127.0.0.1,::1",
			"no_proxy":    "localhost,127.0.0.1,::1",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("buildProxyEnv both =\n%#v\nwant:\n%#v", got, want)
		}
	})

	t.Run("no_proxy merge and deduplication", func(t *testing.T) {
		baseEnv := []string{
			"NO_PROXY=127.0.0.1, corp.local , .internal",
			"no_proxy=CORP.LOCAL,other.local,LOCALHOST",
		}
		got := buildProxyEnv("127.0.0.1:1080", "", ".example.com, other.local, 10.0.0.0/8, ::1", baseEnv)
		wantNoProxy := "localhost,127.0.0.1,::1,corp.local,.internal,other.local,.example.com,10.0.0.0/8"
		if got["NO_PROXY"] != wantNoProxy {
			t.Errorf("NO_PROXY = %q, want %q", got["NO_PROXY"], wantNoProxy)
		}
		if got["no_proxy"] != wantNoProxy {
			t.Errorf("no_proxy = %q, want %q", got["no_proxy"], wantNoProxy)
		}
	})
}

func TestEnvPrintAndShellQuoting(t *testing.T) {
	t.Run("explicit socks5", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runEnvIO([]string{"-socks5", "127.0.0.1:1080", "-print"}, nil, &stdout, &stderr, nil)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("unexpected stderr: %q", stderr.String())
		}
		want := strings.Join([]string{
			"export ALL_PROXY='socks5h://127.0.0.1:1080'",
			"export NO_PROXY='localhost,127.0.0.1,::1'",
			"export all_proxy='socks5h://127.0.0.1:1080'",
			"export no_proxy='localhost,127.0.0.1,::1'",
		}, "\n") + "\n"
		if stdout.String() != want {
			t.Errorf("stdout =\n%s\nwant:\n%s", stdout.String(), want)
		}
	})

	t.Run("explicit http", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runEnvIO([]string{"-http", "127.0.0.1:8080", "-print"}, nil, &stdout, &stderr, nil)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
		}
		want := strings.Join([]string{
			"export ALL_PROXY='http://127.0.0.1:8080'",
			"export HTTPS_PROXY='http://127.0.0.1:8080'",
			"export HTTP_PROXY='http://127.0.0.1:8080'",
			"export NO_PROXY='localhost,127.0.0.1,::1'",
			"export all_proxy='http://127.0.0.1:8080'",
			"export http_proxy='http://127.0.0.1:8080'",
			"export https_proxy='http://127.0.0.1:8080'",
			"export no_proxy='localhost,127.0.0.1,::1'",
		}, "\n") + "\n"
		if stdout.String() != want {
			t.Errorf("stdout =\n%s\nwant:\n%s", stdout.String(), want)
		}
	})

	t.Run("both socks5 and http with no-proxy", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		baseEnv := []string{"NO_PROXY=corp.local"}
		code := runEnvIO([]string{
			"-socks5", "127.0.0.1:1080",
			"-http", "127.0.0.1:8080",
			"-no-proxy", "corp.local,.internal",
			"-print",
		}, baseEnv, &stdout, &stderr, nil)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
		}
		want := strings.Join([]string{
			"export ALL_PROXY='socks5h://127.0.0.1:1080'",
			"export HTTPS_PROXY='http://127.0.0.1:8080'",
			"export HTTP_PROXY='http://127.0.0.1:8080'",
			"export NO_PROXY='localhost,127.0.0.1,::1,corp.local,.internal'",
			"export all_proxy='socks5h://127.0.0.1:1080'",
			"export http_proxy='http://127.0.0.1:8080'",
			"export https_proxy='http://127.0.0.1:8080'",
			"export no_proxy='localhost,127.0.0.1,::1,corp.local,.internal'",
		}, "\n") + "\n"
		if stdout.String() != want {
			t.Errorf("stdout =\n%s\nwant:\n%s", stdout.String(), want)
		}
	})

	t.Run("shell single-quote edge cases", func(t *testing.T) {
		tricky := `a'b,$(id),` + "`uname`" + `;echo "hi"`
		var stdout, stderr bytes.Buffer
		code := runEnvIO([]string{
			"-socks5", "127.0.0.1:1080",
			"-no-proxy", tricky,
			"-print",
		}, nil, &stdout, &stderr, nil)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
		}
		wantVal := `localhost,127.0.0.1,::1,` + tricky
		wantLine := `export NO_PROXY='localhost,127.0.0.1,::1,a'\''b,$(id),` + "`uname`" + `;echo "hi"'`
		if !strings.Contains(stdout.String(), wantLine+"\n") {
			t.Errorf("stdout missing properly escaped line:\ngot:\n%s\nwant substring:\n%s", stdout.String(), wantLine)
		}

		// Verify POSIX /bin/sh actually evaluates the printed exports to exact values.
		if sh, err := exec.LookPath("sh"); err == nil && runtime.GOOS != "windows" {
			cmd := exec.Command(sh, "-c", stdout.String()+`printf '%s' "$NO_PROXY"`)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("sh eval failed: %v", err)
			}
			if string(out) != wantVal {
				t.Errorf("sh eval NO_PROXY = %q, want %q", string(out), wantVal)
			}
		}
	})
}

func TestEnvAutoDiscovery(t *testing.T) {
	t.Run("normalizes non-loopback and empty hosts from status", func(t *testing.T) {
		_, sockPath := startMockControlStatusServer(t, statusDocument{
			Ready:      true,
			Serving:    true,
			SOCKS5Addr: "0.0.0.0:1080",
			HTTPAddr:   ":8080",
		}, http.StatusOK)

		var stdout, stderr bytes.Buffer
		code := runEnvIO([]string{"-socket", sockPath, "-print"}, nil, &stdout, &stderr, nil)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("unexpected stderr: %q", stderr.String())
		}
		for _, want := range []string{
			"export ALL_PROXY='socks5h://127.0.0.1:1080'",
			"export HTTP_PROXY='http://127.0.0.1:8080'",
			"export HTTPS_PROXY='http://127.0.0.1:8080'",
		} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("stdout missing %q:\n%s", want, stdout.String())
			}
		}
	})

	t.Run("discovers via -dir and preserves loopback IPv6", func(t *testing.T) {
		sockDir, _ := startMockControlStatusServer(t, statusDocument{
			Ready:      false,
			Serving:    false,
			SOCKS5Addr: "[::1]:1081",
		}, http.StatusServiceUnavailable)

		var stdout, stderr bytes.Buffer
		code := runEnvIO([]string{"-dir", sockDir, "-print"}, nil, &stdout, &stderr, nil)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "export ALL_PROXY='socks5h://[::1]:1081'\n") {
			t.Errorf("stdout missing IPv6 loopback socks5:\n%s", stdout.String())
		}
	})

	t.Run("discovers via default env socket", func(t *testing.T) {
		_, sockPath := startMockControlStatusServer(t, statusDocument{
			Ready:    true,
			Serving:  true,
			HTTPAddr: "127.0.0.1:8123",
		}, http.StatusOK)
		t.Setenv("TS_UNPLUG_PROXY_SOCK", sockPath)

		var stdout, stderr bytes.Buffer
		code := runEnvIO([]string{"-print"}, nil, &stdout, &stderr, nil)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("unexpected stderr: %q", stderr.String())
		}
		if !strings.Contains(stdout.String(), "export ALL_PROXY='http://127.0.0.1:8123'\n") {
			t.Errorf("stdout missing HTTP ALL_PROXY:\n%s", stdout.String())
		}
	})

	t.Run("forwards-only daemon returns clear error", func(t *testing.T) {
		_, sockPath := startMockControlStatusServer(t, statusDocument{
			Ready:    true,
			Serving:  true,
			Forwards: []string{"127.0.0.1:5432=db.ts.net:5432"},
		}, http.StatusOK)

		var stdout, stderr bytes.Buffer
		code := runEnvIO([]string{"-socket", sockPath, "-print"}, nil, &stdout, &stderr, nil)
		if code != 1 {
			t.Fatalf("exit = %d, want 1 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want empty", stdout.String())
		}
		if !strings.Contains(stderr.String(), "neither -socks5 nor -http") {
			t.Errorf("stderr = %q, want clear error about missing socks5/http", stderr.String())
		}
	})

	t.Run("explicit -socket unreachable fails without fallback", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing.sock")
		var stdout, stderr bytes.Buffer
		code := runEnvIO([]string{"-socket", missing, "-print"}, nil, &stdout, &stderr, nil)
		if code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want empty", stdout.String())
		}
		if strings.Contains(stderr.String(), "defaulting to") {
			t.Errorf("explicit -socket must not fall back; stderr = %q", stderr.String())
		}
	})

	t.Run("explicit -dir unreachable fails without fallback", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runEnvIO([]string{"-dir", t.TempDir(), "-print"}, nil, &stdout, &stderr, nil)
		if code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want empty", stdout.String())
		}
		if strings.Contains(stderr.String(), "defaulting to") {
			t.Errorf("explicit -dir must not fall back; stderr = %q", stderr.String())
		}
	})

	t.Run("default control socket unavailable falls back to 127.0.0.1:1080", func(t *testing.T) {
		t.Setenv("TS_UNPLUG_PROXY_SOCK", "")
		t.Setenv("TS_UNPLUG_PROXY_DIR", "")
		t.Setenv("XDG_STATE_HOME", t.TempDir())

		var stdout, stderr bytes.Buffer
		code := runEnvIO([]string{"-print"}, nil, &stdout, &stderr, nil)
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr=%q)", code, stderr.String())
		}
		wantNote := "ts-unplug-proxy env: control socket unavailable, defaulting to socks5h://127.0.0.1:1080 (pass -socks5/-http or start daemon)"
		if !strings.Contains(stderr.String(), wantNote) {
			t.Errorf("stderr = %q, want fallback note %q", stderr.String(), wantNote)
		}
		if !strings.Contains(stdout.String(), "export ALL_PROXY='socks5h://127.0.0.1:1080'\n") {
			t.Errorf("stdout missing default socks5h export:\n%s", stdout.String())
		}
	})
}

func TestMergeEnv(t *testing.T) {
	base := []string{
		"PATH=/usr/bin:/bin",
		"ALL_PROXY=http://old:1",
		"FOO=bar=baz",
		"ALL_PROXY=http://duplicate:2",
		"all_proxy=http://oldlower:3",
	}
	baseCopy := append([]string(nil), base...)

	overrides := map[string]string{
		"ALL_PROXY": "socks5h://127.0.0.1:1080",
		"all_proxy": "socks5h://127.0.0.1:1080",
		"NO_PROXY":  "localhost,127.0.0.1,::1",
		"no_proxy":  "localhost,127.0.0.1,::1",
	}

	got := mergeEnv(base, overrides)
	want := []string{
		"PATH=/usr/bin:/bin",
		"ALL_PROXY=socks5h://127.0.0.1:1080",
		"FOO=bar=baz",
		"all_proxy=socks5h://127.0.0.1:1080",
		"NO_PROXY=localhost,127.0.0.1,::1",
		"no_proxy=localhost,127.0.0.1,::1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeEnv =\n%#v\nwant:\n%#v", got, want)
	}
	if !reflect.DeepEqual(base, baseCopy) {
		t.Errorf("mergeEnv mutated base slice: %#v", base)
	}
}

func TestEnvExecRunner(t *testing.T) {
	t.Run("passes args and merged env with and without --", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			args    []string
			wantCmd []string
		}{
			{
				name:    "with double dash",
				args:    []string{"-socks5", "127.0.0.1:1080", "-http", "127.0.0.1:8080", "--", "curl", "-I", "https://gxa.ts.net/"},
				wantCmd: []string{"curl", "-I", "https://gxa.ts.net/"},
			},
			{
				name:    "without double dash",
				args:    []string{"-socks5", "127.0.0.1:1080", "-http", "127.0.0.1:8080", "curl", "-I", "https://gxa.ts.net/"},
				wantCmd: []string{"curl", "-I", "https://gxa.ts.net/"},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var gotCmd, gotEnv []string
				execFn := func(cmdArgs []string, env []string) (int, error) {
					gotCmd = cmdArgs
					gotEnv = env
					return 0, nil
				}
				baseEnv := []string{"KEEP=1", "ALL_PROXY=old"}
				var stdout, stderr bytes.Buffer
				code := runEnvIO(tc.args, baseEnv, &stdout, &stderr, execFn)
				if code != 0 {
					t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
				}
				if !reflect.DeepEqual(gotCmd, tc.wantCmd) {
					t.Errorf("cmdArgs = %#v, want %#v", gotCmd, tc.wantCmd)
				}
				envMap := envSliceToMap(gotEnv)
				if envMap["KEEP"] != "1" {
					t.Errorf("KEEP = %q, want 1", envMap["KEEP"])
				}
				if envMap["ALL_PROXY"] != "socks5h://127.0.0.1:1080" {
					t.Errorf("ALL_PROXY = %q", envMap["ALL_PROXY"])
				}
				if envMap["HTTP_PROXY"] != "http://127.0.0.1:8080" {
					t.Errorf("HTTP_PROXY = %q", envMap["HTTP_PROXY"])
				}
				if envMap["HTTPS_PROXY"] != "http://127.0.0.1:8080" {
					t.Errorf("HTTPS_PROXY = %q", envMap["HTTPS_PROXY"])
				}
			})
		}
	})

	t.Run("propagates execFn exit code and error", func(t *testing.T) {
		execFn := func(cmdArgs []string, env []string) (int, error) {
			return 127, errors.New("not found")
		}
		var stdout, stderr bytes.Buffer
		code := runEnvIO([]string{"-socks5", "127.0.0.1:1080", "--", "missing-cmd"}, nil, &stdout, &stderr, execFn)
		if code != 127 {
			t.Errorf("exit = %d, want 127", code)
		}
		if !strings.Contains(stderr.String(), "not found") {
			t.Errorf("stderr = %q, want error message", stderr.String())
		}
	})
}

// TestEnvExecCommandSubprocess exercises the real execCommand implementation
// (syscall.Exec on Unix, exec.Command on non-Unix) inside a subprocess so we
// verify environment variables and exit codes arrive cleanly in the child.
func TestEnvExecCommandSubprocess(t *testing.T) {
	if os.Getenv("TS_UNPLUG_PROXY_TEST_EXEC_HELPER") == "1" {
		os.Exit(runEnv([]string{
			"-socks5", "127.0.0.1:1089",
			"-http", "127.0.0.1:8089",
			"-no-proxy", "corp.test",
			"--",
			"sh", "-c",
			`printf 'ALL=%s\nHTTP=%s\nHTTPS=%s\nNO=%s\nKEEP=%s\n' "$ALL_PROXY" "$HTTP_PROXY" "$HTTPS_PROXY" "$NO_PROXY" "$UNRELATED_VAR"; exit 7`,
		}))
	}

	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh subprocess test requires Unix")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not available: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestEnvExecCommandSubprocess$")
	cmd.Env = append(os.Environ(),
		"TS_UNPLUG_PROXY_TEST_EXEC_HELPER=1",
		"UNRELATED_VAR=preserved-ok",
		"ALL_PROXY=socks5://stale:9999",
		"NO_PROXY=existing.test",
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("subprocess exit err = %v (want exit 7); output:\n%s", err, string(out))
	}
	got := string(out)
	for _, want := range []string{
		"ALL=socks5h://127.0.0.1:1089\n",
		"HTTP=http://127.0.0.1:8089\n",
		"HTTPS=http://127.0.0.1:8089\n",
		"NO=localhost,127.0.0.1,::1,existing.test,corp.test\n",
		"KEEP=preserved-ok\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("subprocess output missing %q;\ngot:\n%s", want, got)
		}
	}

	// Also verify execCommand fails cleanly with exit 127 for a missing binary.
	code, execErr := execCommand([]string{"definitely-no-such-binary-ts-unplug-proxy"}, os.Environ())
	if code != 127 || execErr == nil {
		t.Errorf("execCommand(missing) = (%d, %v), want (127, non-nil error)", code, execErr)
	}
}

func envSliceToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	return m
}
