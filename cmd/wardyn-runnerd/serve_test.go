// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func TestServeValidatesIdentityBeforeDependencies(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "runner")
	key, err := runneridentity.Generate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	identity := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key, Fingerprint: runnerwire.Fingerprint(key.Public().(ed25519.PublicKey)), OrgURLSHA256: federation.OrgURLSHA256("https://org.example")}
	if err := runneridentity.Save(stateDir, identity); err != nil {
		t.Fatal(err)
	}
	called := false
	cmd := newServeCommand(func(context.Context, runneridentity.Identity, string, serveOptions) (runnerRuntime, error) {
		called = true
		return runnerRuntime{}, nil
	})
	cmd.SetArgs([]string{"--org", "https://other.example", "--state-dir", stateDir})
	if err := cmd.Execute(); err == nil || called {
		t.Fatalf("wrong org reached dependencies: %v called=%v", err, called)
	}
}

func TestServeMissingProductionHooksRefuses(t *testing.T) {
	if _, err := productionRunnerHooks(context.Background(), runneridentity.Identity{}, t.TempDir(), serveOptions{}); err == nil {
		t.Fatal("missing durable handlers accepted")
	}
}

func TestServeRecordingModeMatchesCapture(t *testing.T) {
	for _, tc := range []struct {
		mode string
		on   bool
		bad  bool
	}{{"pg", true, false}, {"fs", true, false}, {"off", false, false}, {"unknown", false, true}} {
		got, err := (serveOptions{recording: tc.mode}).recordingEnabled()
		if got != tc.on || (err != nil) != tc.bad {
			t.Fatalf("mode=%s enabled=%v error=%v", tc.mode, got, err)
		}
	}
}

// WARDYN_RELAY_LISTEN reaches the builder, and a builder's refusal (a relay or
// Docker this host cannot use) ends serve before the runner ever dials its org.
func TestServeRefusesBeforeDialingTheOrg(t *testing.T) {
	dialed := false
	org := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { dialed = true }))
	defer org.Close()
	stateDir := filepath.Join(t.TempDir(), "runner")
	key, err := runneridentity.Generate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	identity := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key, Fingerprint: runnerwire.Fingerprint(key.Public().(ed25519.PublicKey)), OrgURLSHA256: federation.OrgURLSHA256(org.URL)}
	if err := runneridentity.Save(stateDir, identity); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARDYN_RELAY_LISTEN", "127.0.0.1")
	refusal := errors.New("relay: unusable")
	var got serveOptions
	cmd := newServeCommand(func(_ context.Context, _ runneridentity.Identity, _ string, opts serveOptions) (runnerRuntime, error) {
		got = opts
		return runnerRuntime{}, refusal
	})
	cmd.SetArgs([]string{"--org", org.URL, "--state-dir", stateDir})
	if err := cmd.Execute(); !errors.Is(err, refusal) || dialed {
		t.Fatalf("serve = %v, dialed=%v; want the builder's refusal and no dial", err, dialed)
	}
	if got.relayListen != "127.0.0.1" {
		t.Fatalf("relayListen = %q, want the WARDYN_RELAY_LISTEN value", got.relayListen)
	}
}

// --upstream-proxy (or WARDYN_UPSTREAM_PROXY) is checked at startup, before anything builds, and an
// error never repeats the URL, which may hold a credential; a valid one reaches the builder as given.
func TestServeUpstreamProxyIsValidatedAtStartup(t *testing.T) {
	org := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer org.Close()
	stateDir := filepath.Join(t.TempDir(), "runner")
	key, err := runneridentity.Generate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := runneridentity.Save(stateDir, runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key,
		Fingerprint: runnerwire.Fingerprint(key.Public().(ed25519.PublicKey)), OrgURLSHA256: federation.OrgURLSHA256(org.URL)}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, flag, env, want string
		bad                   bool
	}{
		{name: "none"},
		{name: "flag", flag: "http://corp.example:3128", want: "http://corp.example:3128"},
		{name: "environment", env: " http://user:pw@corp.example:3128 ", want: "http://user:pw@corp.example:3128"},
		{name: "flag over environment", flag: "http://flag.example:1", env: "http://env.example:2", want: "http://flag.example:1"},
		{name: "wrong scheme", flag: "socks5://user:secret@corp.example:1080", bad: true},
		{name: "bad port", env: "http://user:secret@corp.example:99999", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WARDYN_UPSTREAM_PROXY", tc.env)
			var got string
			built := false
			cmd := newServeCommand(func(_ context.Context, _ runneridentity.Identity, _ string, opts serveOptions) (runnerRuntime, error) {
				built, got = true, opts.upstreamProxy
				return runnerRuntime{}, errors.New("stop")
			})
			args := []string{"--org", org.URL, "--state-dir", stateDir}
			if tc.flag != "" {
				args = append(args, "--upstream-proxy", tc.flag)
			}
			cmd.SetArgs(args)
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})
			err := cmd.Execute()
			switch {
			case tc.bad && (built || err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "upstream-proxy")):
				t.Fatalf("a bad URL built=%v err=%v; want a refusal before building that does not repeat the URL", built, err)
			case !tc.bad && (!built || got != tc.want):
				t.Fatalf("builder saw upstream proxy %q (built=%v), want %q", got, built, tc.want)
			}
		})
	}
}

// serve --help must not print the upstream proxy variable: it may hold a credential, and pflag prints a
// flag's default.
func TestServeHelpNeverPrintsTheUpstreamProxyCredential(t *testing.T) {
	t.Setenv("WARDYN_UPSTREAM_PROXY", "http://user:hunter2@corp.example:3128")
	cmd := newServeCommand(nil)
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "hunter2") || strings.Contains(out.String(), "corp.example") {
		t.Errorf("serve --help shows the upstream proxy variable:\n%s", out.String())
	}
}
