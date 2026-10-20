// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
)

// doctorRig is a doctor run on a temporary home with a fake engine, image lookup and relay probe.
type doctorRig struct {
	home      string
	env       map[string]string
	engineErr error
	imageErr  error
	imageAt   string
	relayErr  error
	relayRan  bool
	noNet     bool
	pulled    bool
	orgPool   *x509.CertPool
	org       string
}

func newRig(t *testing.T) *doctorRig {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","version":"0.10.0"}`))
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &doctorRig{home: t.TempDir(), env: map[string]string{"WARDYN_RUNNER_ORG_URL": srv.URL}, imageAt: "local", orgPool: pool, org: srv.URL}
}

func (r *doctorRig) run(o doctorOptions) doctor.Report {
	h := setupHost{goos: "linux", home: r.home, dockerHost: "unix:///var/run/docker.sock",
		inspect: func(context.Context, relayPair) (dockerEngine, error) {
			if r.engineErr != nil {
				return dockerEngine{}, r.engineErr
			}
			return dockerEngine{desc: `"Ubuntu" (box)`, version: "27.1.0", native: true, gateway: netip.MustParseAddr("172.17.0.1")}, nil
		},
		image:      func(context.Context, string) (string, error) { return r.imageAt, r.imageErr },
		hasNetwork: func(context.Context, string) (bool, error) { return !r.noNet, nil },
		relay: func(context.Context, string, string, dockerEngine, relayPair) (relayPair, error) {
			r.relayRan = true
			return relayPair{bind: netip.MustParseAddr("172.17.0.1"), target: hostGateway}, r.relayErr
		}}
	return runDoctor(context.Background(), h, o, func(k string) string { return r.env[k] }, doctor.Network{
		LookupHost: func(context.Context, string) ([]string, error) { return []string{"203.0.113.7"}, nil },
		ProxyFor:   func(*http.Request) (*url.URL, error) { return nil, nil }, SystemRoots: r.orgPool})
}

func stage(t *testing.T, rep doctor.Report, name string) doctor.Stage {
	t.Helper()
	for _, s := range rep.Stages {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %q stage", name)
	return doctor.Stage{}
}

func TestRunnerDoctorAllGood(t *testing.T) {
	r := newRig(t)
	rep := r.run(doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"})
	for _, s := range rep.Stages {
		if s.Name == "trusted-ca" {
			continue
		}
		if s.Status != doctor.StatusOK {
			t.Errorf("%s = %s: %s %s", s.Name, s.Status, s.Detail, s.Fix)
		}
	}
	if !r.relayRan {
		t.Error("the relay probe never ran")
	}
	if got := stage(t, rep, "engine").Detail; !strings.Contains(got, "27.1.0") || !strings.Contains(got, "unix:///var/run/docker.sock") {
		t.Errorf("engine = %q, want the socket and the version", got)
	}
	for _, s := range rep.Stages {
		if s.Name == "identity" {
			t.Error("a runner's doctor must not sign in as anything")
		}
	}
}

// TestRunnerDoctorStagesNameTheirSetting fakes one failure per runner stage.
func TestRunnerDoctorStagesNameTheirSetting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rig     func(*doctorRig)
		opts    doctorOptions
		stage   string
		setting string
		in      string
	}{
		{"engine does not answer", func(r *doctorRig) { r.engineErr = errors.New("docker engine not reachable: connection refused") }, doctorOptions{}, "engine", "DOCKER_HOST", "not reachable"},
		{"image cannot be found", func(r *doctorRig) { r.imageErr = errors.New("manifest unknown") }, doctorOptions{proxyImage: "mirror.example.com/wardyn-proxy:1"}, "proxy-image", "WARDYN_PROXY_IMAGE", "mirror.example.com/wardyn-proxy:1 cannot be found"},
		{"pinned image of another release", func(r *doctorRig) { r.env["WARDYN_PROXY_IMAGE_PINNED"] = "1" }, doctorOptions{proxyImage: releaseProxyRepo + ":0.9.5"}, "proxy-image", "WARDYN_PROXY_IMAGE", "version skew"},
		{"relay not reachable", func(r *doctorRig) {
			r.relayErr = errors.New("relay: no container on Docker engine reached this runner")
		}, doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"}, "relay", "WARDYN_RELAY_LISTEN", "no container"},
		{"relay bind not allowed", func(*doctorRig) {}, doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0", relayListen: "0.0.0.0"}, "relay", "WARDYN_RELAY_LISTEN", "refusing to listen on 0.0.0.0"},
		{"relay listen unreadable", func(*doctorRig) {}, doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0", relayListen: "not-an-address"}, "relay", "WARDYN_RELAY_LISTEN", "--relay-listen"},
		{"relay listen from the service's settings", func(r *doctorRig) {
			dir := filepath.Join(r.home, ".config", "wardyn")
			_ = os.MkdirAll(dir, 0o700)
			_ = os.WriteFile(filepath.Join(dir, "runnerd.env"), []byte("WARDYN_RELAY_LISTEN=192.0.2.9\n"), 0o600)
		}, doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"}, "relay", "WARDYN_RELAY_LISTEN", "refusing to listen on 192.0.2.9"},
		{"organisation certificate no root signed", func(r *doctorRig) { r.orgPool = x509.NewCertPool() }, doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"}, "tls", "WARDYN_TRUSTED_CA_FILE", "no trusted root signed"},
		{"proxy network missing", func(r *doctorRig) { r.noNet = true }, doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"}, "relay", "WARDYN_INTERNAL_NETWORK", "wardyn-internal that run proxies attach to does not exist"},
		{"organisation URL not https", func(r *doctorRig) { r.env["WARDYN_RUNNER_ORG_URL"] = "http://wardyn.example.com" }, doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"}, "org-url", "WARDYN_RUNNER_ORG_URL", "https://"},
		{"no organisation set", func(r *doctorRig) { delete(r.env, "WARDYN_RUNNER_ORG_URL") }, doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"}, "org-url", "WARDYN_RUNNER_ORG_URL", "no organisation"},
		{"CA bundle unreadable", func(*doctorRig) {}, doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0", trustedCAFile: "/nonexistent/ca.pem"}, "trusted-ca", "WARDYN_TRUSTED_CA_FILE", "/nonexistent/ca.pem"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			tc.rig(r)
			rep := r.run(tc.opts)
			s := stage(t, rep, tc.stage)
			if s.Status != doctor.StatusFail || s.Setting != tc.setting || !strings.Contains(s.Detail+s.Fix, tc.in) || !strings.Contains(s.Fix, "set "+tc.setting+" to") {
				t.Errorf("%s = %s setting %q detail %q fix %q, want FAIL naming %s and %q", tc.stage, s.Status, s.Setting, s.Detail, s.Fix, tc.setting, tc.in)
			}
		})
	}
}

func TestMissingProxyNetworkSkipsTheRelayProbe(t *testing.T) {
	r := newRig(t)
	r.noNet = true
	r.env["WARDYN_INTERNAL_NETWORK"] = "corp-proxies"
	rep := r.run(doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"})
	if got := stage(t, rep, "relay").Detail; r.relayRan || !strings.Contains(got, "corp-proxies") {
		t.Errorf("relayRan = %v, relay detail %q, want no probe and the configured network named", r.relayRan, got)
	}
}

func TestUnpinnedImageOfAnotherReleaseIsNoSkew(t *testing.T) {
	r := newRig(t)
	r.env["WARDYN_PROXY_IMAGE"] = releaseProxyRepo + ":0.9.5"
	s := stage(t, r.run(doctorOptions{}), "proxy-image")
	// A release image of this project that setup did not mark as pinned is what the organisation's answer replaces.
	if s.Status != doctor.StatusOK {
		t.Errorf("proxy-image = %s %q: an unpinned image follows the organisation, so it is not skew", s.Status, s.Detail)
	}
}

func TestDoctorNeverPullsOrStartsAProbeForAnImageNotOnTheEngine(t *testing.T) {
	r := newRig(t)
	r.imageAt = "registry"
	rep := r.run(doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"})
	if r.relayRan {
		t.Error("the relay probe ran with the image absent from the engine")
	}
	if s := stage(t, rep, "relay"); s.Status != doctor.StatusSkip || !strings.Contains(s.Detail, "never pulls") {
		t.Errorf("relay = %s %q, want a skip that says doctor never pulls", s.Status, s.Detail)
	}
	if s := stage(t, rep, "proxy-image"); s.Status != doctor.StatusOK || !strings.Contains(s.Detail, "registry") {
		t.Errorf("proxy-image = %s %q", s.Status, s.Detail)
	}
}

func TestRunnerOnlyRunsNoNetworkStages(t *testing.T) {
	r := newRig(t)
	rep := r.run(doctorOptions{runnerOnly: true, proxyImage: releaseProxyRepo + ":0.10.0"})
	var names []string
	for _, s := range rep.Stages {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "engine,proxy-image,relay" {
		t.Errorf("stages = %s, want only the runner's", got)
	}
}

func TestRunnerOnlyTakesTheServerVersionForTheSkewCheck(t *testing.T) {
	r := newRig(t)
	r.env["WARDYN_PROXY_IMAGE_PINNED"] = "1"
	s := stage(t, r.run(doctorOptions{runnerOnly: true, serverVersion: "0.10.0", proxyImage: releaseProxyRepo + ":0.9.5"}), "proxy-image")
	if s.Status != doctor.StatusFail || !strings.Contains(s.Detail, "version skew") {
		t.Errorf("proxy-image = %s %q, want version skew", s.Status, s.Detail)
	}
}

// TestDoctorWritesNothing checks a home that holds a service's settings and an identity directory is
// byte-identical after a run, and that no file appeared beside the process either.
func TestDoctorWritesNothing(t *testing.T) {
	r := newRig(t)
	cfg := filepath.Join(r.home, ".config", "wardyn")
	if err := os.MkdirAll(filepath.Join(cfg, "runner"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"runnerd.env": "WARDYN_PROXY_IMAGE=mirror.example.com/p:1\n", "runner/runner.json": "{}"} {
		if err := os.WriteFile(filepath.Join(cfg, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := digest(t, r.home)
	r.run(doctorOptions{})
	r.imageAt, r.engineErr = "registry", errors.New("down")
	r.run(doctorOptions{runnerOnly: true})
	if after := digest(t, r.home); after != before {
		t.Errorf("the home changed:\nbefore %s\nafter  %s", before, after)
	}
}

func digest(t *testing.T, root string) string {
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

func TestDoctorNeverPrintsAProxyCredential(t *testing.T) {
	r := newRig(t)
	r.engineErr = errors.New("cannot reach ssh://deploy:hunter2-secret@engine.example.com")
	r.env["WARDYN_PROXY_IMAGE"] = "registry.example.com/p:1"
	rep := r.run(doctorOptions{})
	var text, js strings.Builder
	_ = rep.WriteText(&text)
	_ = rep.WriteJSON(&js)
	for _, out := range []string{text.String(), js.String()} {
		if strings.Contains(out, "hunter2-secret") || strings.Contains(out, "deploy:") {
			t.Errorf("the output carries a credential:\n%s", out)
		}
	}
}

// TestRunnerDoctorPlainOutputGolden pins the plain lines of a run that fails in the engine's stages.
func TestRunnerDoctorPlainOutputGolden(t *testing.T) {
	r := newRig(t)
	r.relayErr = errors.New("relay: no container on Docker engine \"Ubuntu\" (box) (native) reached this runner:\n  listen 172.17.0.1, reach at host-gateway: refused\n" +
		"Set --relay-listen (WARDYN_RELAY_LISTEN) and --relay-host-target (WARDYN_RELAY_HOST_TARGET); see docs/runner-registration.md#requirements.")
	rep := r.run(doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"})
	var b strings.Builder
	if err := rep.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	got := strings.ReplaceAll(b.String(), strings.TrimPrefix(r.org, "https://"), "ORG")
	want, err := os.ReadFile("testdata/doctor_runner.golden")
	if os.Getenv("WARDYN_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile("testdata/doctor_runner.golden", []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		want = []byte(got)
	} else if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("plain output differs from testdata/doctor_runner.golden:\n%s", got)
	}
}
