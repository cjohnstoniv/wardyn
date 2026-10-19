// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// isolateProfile points the CLI profile at an empty temporary configuration directory and clears the
// URL environment, so a test never reads the developer's own profile. It returns the profile's directory.
func isolateProfile(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("HOME", cfg)
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("WARDYN_URL", "")
	t.Setenv("WARDYN_CLI_ALLOW_PLAINTEXT", "")
	t.Setenv("WARDYN_ADMIN_TOKEN", "")
	t.Setenv("WARDYN_TOKEN", "")
	dir, err := cliprofile.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, cfg) {
		t.Fatalf("profile directory %s is outside the temporary configuration directory %s", dir, cfg)
	}
	return dir
}

// execCaptureErr runs the CLI and returns what it wrote to stderr.
func execCaptureErr(t *testing.T, args ...string) (stderr string, err error) {
	t.Helper()
	root := rootCmd()
	root.SetArgs(args)
	var errOut strings.Builder
	root.SetOut(&strings.Builder{})
	root.SetErr(&errOut)
	err = root.Execute()
	return errOut.String(), err
}

// TestAuthSetURL pins `wardyn auth set-url`: it saves the normalised URL, and for a plain-http,
// non-loopback one it warns exactly as a command's token warning does (silenced by
// WARDYN_CLI_ALLOW_PLAINTEXT) and never refuses.
func TestAuthSetURL(t *testing.T) {
	for _, tc := range []struct {
		name, arg, env, want string
		warns                bool
	}{
		{"https is saved as its origin and base path", " https://wardyn.example.com/w/ ", "", "https://wardyn.example.com/w", false},
		{"loopback http does not warn", "http://localhost:8080", "", "http://localhost:8080", false},
		{"plain http to another host warns and is still saved", "http://wardyn.example.com", "", "http://wardyn.example.com", true},
		{"WARDYN_CLI_ALLOW_PLAINTEXT silences the warning and still saves", "http://wardyn.example.com", "1", "http://wardyn.example.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolateProfile(t)
			t.Setenv("WARDYN_CLI_ALLOW_PLAINTEXT", tc.env)
			stderr, err := execCaptureErr(t, "auth", "set-url", tc.arg)
			if err != nil {
				t.Fatalf("auth set-url refused %q: %v (it warns, it never refuses)", tc.arg, err)
			}
			if warned := strings.Contains(stderr, "WARNING"); warned != tc.warns {
				t.Errorf("warned = %v, want %v (stderr %q)", warned, tc.warns, stderr)
			}
			if got, err := cliprofile.Read(dir); err != nil || got != tc.want {
				t.Errorf("profile = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	t.Run("a value that is no URL is refused and saves nothing", func(t *testing.T) {
		dir := isolateProfile(t)
		if _, err := execCaptureErr(t, "auth", "set-url", "wardyn.example.com"); err == nil {
			t.Fatal("auth set-url accepted a value with no scheme")
		}
		if _, err := os.Stat(filepath.Join(dir, cliprofile.FileName)); err == nil {
			t.Error("a refused set-url still wrote the profile")
		}
	})
}

// TestURLPrecedence pins --url > WARDYN_URL > profile > the built-in default, each against a real server
// so the request itself shows which URL a command dialed.
func TestURLPrecedence(t *testing.T) {
	dir := isolateProfile(t)
	flagSrv := newCmdServer(t, http.StatusOK, []types.AgentRun{})
	envSrv := newCmdServer(t, http.StatusOK, []types.AgentRun{})
	profileSrv := newCmdServer(t, http.StatusOK, []types.AgentRun{})
	if err := cliprofile.Write(dir, profileSrv.URL); err != nil {
		t.Fatal(err)
	}
	dialed := func(srv *cmdServer) int { srv.mu.Lock(); defer srv.mu.Unlock(); return len(srv.reqs) }

	if err := execCmd(t, "run", "list"); err != nil || dialed(profileSrv) != 1 {
		t.Fatalf("profile only: err=%v, profile server saw %d requests, want 1", err, dialed(profileSrv))
	}
	t.Setenv("WARDYN_URL", envSrv.URL)
	if err := execCmd(t, "run", "list"); err != nil || dialed(envSrv) != 1 || dialed(profileSrv) != 1 {
		t.Fatalf("env over profile: err=%v, env saw %d, profile saw %d; want 1 and 1", err, dialed(envSrv), dialed(profileSrv))
	}
	if err := execCmd(t, "run", "list", "--url", flagSrv.URL); err != nil || dialed(flagSrv) != 1 || dialed(envSrv) != 1 {
		t.Fatalf("flag over env: err=%v, flag saw %d, env saw %d; want 1 and 1", err, dialed(flagSrv), dialed(envSrv))
	}
}

// TestURLPrecedence_DefaultWithNoProfile pins the last rung and its hint: with no flag, no env and no
// profile the CLI dials the built-in default and a refused dial names `wardyn auth set-url`; a URL the
// person named, by the profile or the environment, gets no such line.
func TestURLPrecedence_DefaultWithNoProfile(t *testing.T) {
	dir := isolateProfile(t)
	refused := &url.Error{Op: "Get", URL: defaultURL, Err: errors.New("connection refused")}

	root := rootCmd()
	root.SetArgs([]string{"auth"})
	root.SetOut(&strings.Builder{})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !urlFromDefault {
		t.Fatal("no flag, env or profile, yet the URL is not marked as the built-in default")
	}
	if hint := noOrgHint(refused, urlFromDefault); !strings.Contains(hint, "wardyn auth set-url") {
		t.Errorf("hint for a refused dial against the default = %q, want it to name `wardyn auth set-url`", hint)
	}
	if hint := noOrgHint(errors.New("not a dial failure"), true); hint != "" {
		t.Errorf("hint for an error that is no dial failure = %q, want none", hint)
	}

	if err := cliprofile.Write(dir, "https://wardyn.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "auth"); err != nil {
		t.Fatal(err)
	}
	if urlFromDefault {
		t.Error("a profile names the organisation, yet the URL is marked as the built-in default")
	}
	if hint := noOrgHint(refused, urlFromDefault); hint != "" {
		t.Errorf("hint for a refused dial against a named URL = %q, want none", hint)
	}
}
