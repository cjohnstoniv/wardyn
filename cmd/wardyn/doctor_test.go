// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
)

const doctorToken = "tok-secret-1234"

func doctorHandler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		_, _ = w.Write([]byte(`{"status":"ok","version":"0.10.0"}`))
	case "/api/v1/me":
		if r.Header.Get("Authorization") != "Bearer "+doctorToken {
			http.Error(w, `{"error":"sign in","reason":"sign_in_required"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"method":"token","role":"admin","principal":"someone","email":"a@b.c"}`))
	default:
		http.NotFound(w, r)
	}
}

// doctorEnv isolates the command from this computer: an empty home with no CLI profile and no runner, no
// proxy, and the token only in WARDYN_TOKEN.
func doctorEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for k, v := range map[string]string{"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"), "XDG_DATA_HOME": filepath.Join(home, ".local", "share"),
		"WARDYN_URL": "", "WARDYN_ADMIN_TOKEN": "", "WARDYN_TOKEN": doctorToken, "WARDYN_TRUSTED_CA_FILE": "",
		"HTTPS_PROXY": "", "https_proxy": "", "HTTP_PROXY": "", "http_proxy": "", "ALL_PROXY": "", "NO_PROXY": ""} {
		t.Setenv(k, v)
	}
	return home
}

func runDoctorCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return execCmdCapture(t, append([]string{"doctor"}, args...)...)
}

func TestDoctorPlainOutputGolden(t *testing.T) {
	doctorEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(doctorHandler))
	t.Cleanup(srv.Close)
	out, err := runDoctorCmd(t, "--url", srv.URL)
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	got := strings.ReplaceAll(out, strings.TrimPrefix(srv.URL, "http://"), "ORG")
	golden := filepath.Join("testdata", "doctor_plain.golden")
	if os.Getenv("WARDYN_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("plain output differs from %s:\n%s", golden, got)
	}
}

func TestDoctorUnknownAuthorityExitsOneAndNamesTheSetting(t *testing.T) {
	doctorEnv(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(doctorHandler))
	t.Cleanup(srv.Close)
	out, err := runDoctorCmd(t, "--url", srv.URL)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err = %v, want exit code 1", err)
	}
	for _, want := range []string{"FAIL  tls", "set SSL_CERT_FILE to", "Acme Co", "skip  healthz", "skip  identity"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorSaysWhereTheURLCameFrom(t *testing.T) {
	home := doctorEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(doctorHandler))
	t.Cleanup(srv.Close)
	cfg := filepath.Join(home, ".config", "wardyn")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	profile, _ := json.Marshal(map[string]string{"url": srv.URL})
	if err := os.WriteFile(filepath.Join(cfg, "cli.json"), profile, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, env, flag, want string }{
		{"flag", "", srv.URL, "(from --url)"},
		{"environment", srv.URL, "", "(from WARDYN_URL)"},
		{"profile", "", "", "(from the CLI profile that wardyn login saved)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WARDYN_URL", tc.env)
			args := []string{}
			if tc.flag != "" {
				args = append(args, "--url", tc.flag)
			}
			out, _ := runDoctorCmd(t, args...)
			if !strings.Contains(out, tc.want) {
				t.Errorf("output lacks %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestDoctorNamesWhereTheTokenCameFrom(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		set        func(t *testing.T)
		args       []string
	}{
		{"flag", "the token from --token", func(t *testing.T) { t.Setenv("WARDYN_TOKEN", "") }, []string{"--token", doctorToken}},
		{"admin environment", "the token from WARDYN_ADMIN_TOKEN", func(t *testing.T) { t.Setenv("WARDYN_TOKEN", ""); t.Setenv("WARDYN_ADMIN_TOKEN", doctorToken) }, nil},
		{"personal environment", "the token from WARDYN_TOKEN", func(*testing.T) {}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doctorEnv(t)
			tc.set(t)
			srv := httptest.NewServer(http.HandlerFunc(doctorHandler))
			t.Cleanup(srv.Close)
			out, err := runDoctorCmd(t, append([]string{"--url", srv.URL}, tc.args...)...)
			if err != nil || !strings.Contains(out, tc.want) || strings.Contains(out, doctorToken) {
				t.Errorf("err = %v, want %q and no token value in:\n%s", err, tc.want, out)
			}
		})
	}
}

func TestDoctorTokenRefusedNamesSignIn(t *testing.T) {
	doctorEnv(t)
	t.Setenv("WARDYN_TOKEN", "wrong-token-9999")
	srv := httptest.NewServer(http.HandlerFunc(doctorHandler))
	t.Cleanup(srv.Close)
	out, err := runDoctorCmd(t, "--url", srv.URL)
	if err == nil || !strings.Contains(out, "FAIL  identity") || !strings.Contains(out, "set WARDYN_TOKEN to a personal API token") || strings.Contains(out, "wrong-token-9999") {
		t.Errorf("err = %v, output:\n%s", err, out)
	}
}

func TestDoctorJSONCarriesTheStagesAndNoToken(t *testing.T) {
	doctorEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(doctorHandler))
	t.Cleanup(srv.Close)
	out, err := runDoctorCmd(t, "--url", srv.URL, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		OK     bool
		Stages []doctor.Stage
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || !got.OK {
		t.Fatalf("json: %v\n%s", err, out)
	}
	var names []string
	for _, s := range got.Stages {
		names = append(names, s.Name)
	}
	if want := "org-url,dns,proxy,tls,healthz,identity,runner"; strings.Join(names, ",") != want {
		t.Errorf("stages = %v, want %s", names, want)
	}
	if strings.Contains(out, doctorToken) {
		t.Errorf("the JSON carries the token:\n%s", out)
	}
}

// TestDoctorWritesNothing checks that a home with a CLI profile and a runner's files is byte-identical after a
// run that fails and one that passes.
func TestDoctorWritesNothing(t *testing.T) {
	home := doctorEnv(t)
	cfg := filepath.Join(home, ".config", "wardyn")
	if err := os.MkdirAll(filepath.Join(cfg, "runner"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"cli.json": `{"url":"http://127.0.0.1:1"}`, "runner/runner.json": "{}", "runnerd.env": "WARDYN_PROXY_IMAGE=x\n"} {
		if err := os.WriteFile(filepath.Join(cfg, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(doctorHandler))
	plain := httptest.NewServer(http.HandlerFunc(doctorHandler))
	t.Cleanup(tlsSrv.Close)
	t.Cleanup(plain.Close)
	before := homeDigest(t, home)
	for _, url := range []string{tlsSrv.URL, plain.URL, "http://127.0.0.1:1"} {
		_, _ = runDoctorCmd(t, "--url", url)
		_, _ = runDoctorCmd(t, "--url", url, "--json")
	}
	if after := homeDigest(t, home); after != before {
		t.Errorf("the home changed:\nbefore %s\nafter  %s", before, after)
	}
}

func homeDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, _ := d.Info()
		fmt.Fprintf(h, "%s %v %v\n", path, fi.Mode(), fi.ModTime().UnixNano())
		if !d.IsDir() {
			b, _ := os.ReadFile(path)
			h.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// installRunnerd writes a stand-in for the installed runner binary that prints out and exits with code, and
// fails the test's run when it is handed a WARDYN_* variable.
func installRunnerd(t *testing.T, home, out string, code int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no runner on Windows")
	}
	path := doctor.RunnerdPath(runtime.GOOS, home, os.Getenv("XDG_DATA_HOME"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nif env | grep -q '^WARDYN_'; then echo '{\"stages\":[{\"name\":\"leak\",\"status\":\"FAIL\",\"detail\":\"a WARDYN_ variable reached the runner\"}]}'; exit 1; fi\n"+
		"echo \"$@\" > %q\ncat <<'EOF'\n%s\nEOF\nexit %d\n", filepath.Join(home, "args.txt"), out, code)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorRunsAnInstalledRunnersStages(t *testing.T) {
	home := doctorEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(doctorHandler))
	t.Cleanup(srv.Close)
	installRunnerd(t, home, `{"ok":false,"stages":[{"name":"engine","status":"FAIL","detail":"no answer from ssh://deploy:hunter2-secret@engine.example.com","setting":"DOCKER_HOST","fix":"set DOCKER_HOST to the engine's socket"}]}`, 1)
	out, err := runDoctorCmd(t, "--url", srv.URL)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err = %v, want exit 1 for the runner's failed stage", err)
	}
	if !strings.Contains(out, "FAIL  engine") || !strings.Contains(out, "set DOCKER_HOST to the engine's socket") || strings.Contains(out, "leak") || strings.Contains(out, "hunter2-secret") {
		t.Errorf("output:\n%s", out)
	}
	args, _ := os.ReadFile(filepath.Join(home, "args.txt"))
	if got := strings.TrimSpace(string(args)); got != "doctor --runner-only --json --server-version 0.10.0" {
		t.Errorf("the runner was run as %q", got)
	}
}

func TestDoctorSaysWhenTheInstalledRunnerSaysNothing(t *testing.T) {
	home := doctorEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(doctorHandler))
	t.Cleanup(srv.Close)
	installRunnerd(t, home, "not json", 1)
	out, err := runDoctorCmd(t, "--url", srv.URL)
	if err == nil || !strings.Contains(out, "FAIL  runner") || !strings.Contains(out, "set WARDYN_RUNNER_STATE_DIR to") {
		t.Errorf("err = %v, output:\n%s", err, out)
	}
}

func TestDoctorSkipsTheRunnerWhenNoneIsInstalled(t *testing.T) {
	doctorEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(doctorHandler))
	t.Cleanup(srv.Close)
	out, err := runDoctorCmd(t, "--url", srv.URL)
	if err != nil || !strings.Contains(out, "skip  runner       no runner is installed on this computer") {
		t.Errorf("err = %v, output:\n%s", err, out)
	}
}
