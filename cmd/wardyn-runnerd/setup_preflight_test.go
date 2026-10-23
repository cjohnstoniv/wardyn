// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// machineDependent matches what a setup prints from this computer's own measurements and the key it just made.
var machineDependent = regexp.MustCompile(`(?m)^(this computer reports |registered |  limits: ).*$|[0-9a-f]{64}`)

// TestSetupSuccessOutputGolden pins everything a first setup prints, with the temporary home and the
// organisation's address replaced by names.
func TestSetupSuccessOutputGolden(t *testing.T) {
	f := newSetupFixture(t, "linux")
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := strings.NewReplacer(filepath.Dir(f.host.home), "ROOT", f.org, "ORG", strings.TrimPrefix(f.org, "http://"), "ORG").Replace(out)
	got = machineDependent.ReplaceAllString(got, "${1}MEASURED")
	want, err := os.ReadFile("testdata/setup_success.golden")
	if os.Getenv("WARDYN_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile("testdata/setup_success.golden", []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		want = []byte(got)
	} else if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("setup output differs from testdata/setup_success.golden:\n%s", got)
	}
}

// proxyTo is a proxy for the plain-http organisation of a setupFixture: handle answers each request it is
// sent, with the request already in proxy form.
func proxyTo(t *testing.T, f *setupFixture, handle http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handle)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	f.host.network.ProxyFor = func(*http.Request) (*url.URL, error) { return u, nil }
}

// stripsUpgrade forwards a request as a proxy that drops the WebSocket upgrade headers does.
func stripsUpgrade(w http.ResponseWriter, r *http.Request) {
	out, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// TestSetupRefusesWithDoctorsMessageWhenTheWireFails pins that setup's preflight runs doctor's connection
// stages: a failing stage that makes setup pointless stops it, with the message doctor prints for it, before
// the organisation is asked to register anything.
func TestSetupRefusesWithDoctorsMessageWhenTheWireFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rig     func(*testing.T, *setupFixture)
		stage   string
		setting string
		in      string
	}{
		{"a proxy answers 407", func(t *testing.T, f *setupFixture) {
			proxyTo(t, f, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusProxyAuthRequired) })
		}, "healthz", "HTTP_PROXY", "answered 407: it wants proxy authentication"},
		{"healthz redirects", func(_ *testing.T, f *setupFixture) {
			f.healthz = func(w http.ResponseWriter) {
				w.Header().Set("Location", "https://login.example.com/")
				w.WriteHeader(http.StatusFound)
			}
		}, "healthz", "WARDYN_RUNNER_ORG_URL", "redirected (302) to login.example.com"},
		{"healthz answers 200 with a page", func(_ *testing.T, f *setupFixture) {
			f.healthz = func(w http.ResponseWriter) { _, _ = w.Write([]byte("<html>welcome</html>")) }
		}, "healthz", "WARDYN_RUNNER_ORG_URL", "not Wardyn's health report"},
		{"a gateway strips the WebSocket upgrade", func(_ *testing.T, f *setupFixture) { f.stripUpgrade = true },
			"wire", "WARDYN_RUNNER_ORG_URL", "answered 426 instead of switching to a WebSocket"},
		{"a proxy strips the WebSocket upgrade", func(t *testing.T, f *setupFixture) { proxyTo(t, f, stripsUpgrade) },
			"wire", "HTTP_PROXY", "answered 426 instead of switching to a WebSocket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSetupFixture(t, "linux")
			tc.rig(t, f)
			out, err := f.run(setupOptions{tokenFile: f.tokenFile})
			want := "the " + tc.stage + " stage failed, so setup stops here: "
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), tc.in) ||
				!strings.Contains(err.Error(), "set "+tc.setting+" to") || !strings.Contains(err.Error(), "no token was spent") {
				t.Fatalf("err = %v, want %q naming %s and %q", err, want, tc.setting, tc.in)
			}
			if !strings.Contains(out, "FAIL  "+tc.stage) {
				t.Errorf("the output does not show the failed stage:\n%s", out)
			}
			if f.count("POST /api/v1/runners/register") != 0 || contains(f.events, "install") || contains(f.events, "register") || contains(f.events, "service") {
				t.Errorf("a refused setup still acted: calls %v, steps %v", f.calls, f.events)
			}
			if _, err := os.Stat(mustPaths(t, f).installed); err == nil {
				t.Error("a refused setup installed the binary")
			}
		})
	}
}

// TestSetupWarnsAndGoesOnWhenAStageThatDoesNotStopItFails pins the other half: the runner's last recorded
// failure is history to a setup that is about to restart the service, so it warns and registers.
func TestSetupWarnsAndGoesOnWhenAStageThatDoesNotStopItFails(t *testing.T) {
	f := newSetupFixture(t, "linux")
	p := mustPaths(t, f)
	if err := os.MkdirAll(p.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(offlineAt("dns", "lookup wardyn.example.com: no such host"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath(p.stateDir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "FAIL  status") || !strings.Contains(out, "warning: the status stage failed; setup goes on") {
		t.Errorf("the failed status stage is not shown as a warning:\n%s", out)
	}
	if f.count("POST /api/v1/runners/register") != 1 {
		t.Errorf("setup did not register after the warning (%d)", f.count("POST /api/v1/runners/register"))
	}
}

// TestSetupWarnsOfAPinnedProxyImageOfAnotherRelease pins doctor's version-skew line in setup: the pinned image
// is a named choice, so the line is a warning and the registration goes on.
func TestSetupWarnsOfAPinnedProxyImageOfAnotherRelease(t *testing.T) {
	f := newSetupFixture(t, "linux")
	out, err := f.run(setupOptions{tokenFile: f.tokenFile, proxyImage: releaseProxyRepo + ":0.9.5"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "FAIL  proxy-image") || !strings.Contains(out, "version skew: the pinned image "+releaseProxyRepo+":0.9.5 is release 0.9.5 but the organisation is release 0.10.0") ||
		!strings.Contains(out, "warning: the proxy-image stage failed; setup goes on") {
		t.Errorf("no skew warning:\n%s", out)
	}
	if f.count("POST /api/v1/runners/register") != 1 {
		t.Errorf("setup did not register after the warning (%d)", f.count("POST /api/v1/runners/register"))
	}
}

// TestSetupGoesOnWhenHealthzAnswersAPlainError pins that /healthz, which the runner never calls, does not stop
// a setup whose wire works: a gateway may route the API and answer nothing there. The line is a warning, and the
// registration goes on.
func TestSetupGoesOnWhenHealthzAnswersAPlainError(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			f := newSetupFixture(t, "linux")
			f.healthz = func(w http.ResponseWriter) { w.WriteHeader(code) }
			out, err := f.run(setupOptions{tokenFile: f.tokenFile})
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			for _, want := range []string{"FAIL  healthz", "ok    wire", "warning: the healthz stage failed; setup goes on"} {
				if !strings.Contains(out, want) {
					t.Errorf("no %q in:\n%s", want, out)
				}
			}
			if f.count("POST /api/v1/runners/register") != 1 {
				t.Errorf("setup did not register (%d)", f.count("POST /api/v1/runners/register"))
			}
		})
	}
}
