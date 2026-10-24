// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
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

func TestReleaseProxyImageIsTheVersionsOwn(t *testing.T) {
	for version, want := range map[string]string{
		"0.9.1":             "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.1",
		"0.9.1-rc.3":        "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.1-rc.3",
		"0.9.1+abc123def":   "",
		"0.9.1+abc-dirty":   "",
		"0.9.1-rc.3+abc123": "",
	} {
		old := buildVersion
		buildVersion = func() string { return version }
		got := releaseProxyImage()
		buildVersion = old
		if got != want {
			t.Errorf("%s: %q, want %q", version, got, want)
		}
	}
}

// serve's proxy image is, in order, --proxy-image, WARDYN_PROXY_IMAGE, the image the organisation named
// at registration (runner.json), and this binary's release image.
func TestServeProxyImagePrecedence(t *testing.T) {
	old := buildVersion
	buildVersion = func() string { return "0.9.1" }
	t.Cleanup(func() { buildVersion = old })
	const release, fromFlag, fromEnv, fromIdentity = "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.1", "flag/proxy:1", "env/proxy:1", "mirror/proxy:1"
	for _, tc := range []struct {
		name, flag, env, identity, want string
	}{
		{"flag", fromFlag, fromEnv, fromIdentity, fromFlag},
		{"environment", "", fromEnv, fromIdentity, fromEnv},
		{"identity", "", "", fromIdentity, fromIdentity},
		{"release", "", "", "", release},
		// An unmarked release image in the environment is what a setup before 0.9.3 derived, not a pin.
		{"stale derived environment", "", "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.0", fromIdentity, fromIdentity},
		// A setup writes the image the organisation named unmarked; it is that answer, not a pin.
		{"environment equal to the identity's", "", fromIdentity, fromIdentity, fromIdentity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			org := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			defer org.Close()
			stateDir := filepath.Join(t.TempDir(), "runner")
			key, err := runneridentity.Generate(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			id := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key, Fingerprint: runnerwire.Fingerprint(key.Public().(ed25519.PublicKey)),
				OrgURLSHA256: federation.OrgURLSHA256(org.URL), ProxyImage: tc.identity}
			if err := runneridentity.Save(stateDir, id); err != nil {
				t.Fatal(err)
			}
			t.Setenv("WARDYN_PROXY_IMAGE", tc.env)
			var got string
			cmd := newServeCommand(func(_ context.Context, _ runneridentity.Identity, _ string, opts serveOptions) (runnerRuntime, error) {
				got = opts.proxyImage
				return runnerRuntime{}, errors.New("stop")
			})
			args := []string{"--org", org.URL, "--state-dir", stateDir}
			if tc.flag != "" {
				args = append(args, "--proxy-image", tc.flag)
			}
			cmd.SetArgs(args)
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})
			_ = cmd.Execute()
			if got != tc.want {
				t.Errorf("serve ran with proxy image %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProxyImagePinned(t *testing.T) {
	none := func(string) string { return "" }
	marked := func(k string) string {
		if k == "WARDYN_PROXY_IMAGE_PINNED" {
			return "1"
		}
		return ""
	}
	const release, mirror = "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.0", "mirror.corp/wardyn-proxy:0.9.0"
	for _, tc := range []struct {
		name, image, identity string
		flag                  bool
		getenv                func(string) string
		want                  bool
	}{
		{"nothing named", "", "", true, marked, false},
		{"flag", release, "", true, none, true},
		{"marked by setup", release, "", false, marked, true},
		{"unmarked release image", release, "", false, none, false},
		{"unmarked mirror", mirror, "", false, none, true},
		{"the image the organisation named at registration", mirror, mirror, false, none, false},
		{"marked, though it is the organisation's", mirror, mirror, false, marked, true},
	} {
		if got := proxyImagePinned(tc.image, tc.identity, tc.flag, tc.getenv); got != tc.want {
			t.Errorf("%s: pinned = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The organisation's answer at connect moves an unpinned runner to its image, once per change, ignores
// an unusable answer, and never touches a pinned runner.
func TestFollowOrgProxyImage(t *testing.T) {
	var set []string
	follow := followOrgProxyImage("org/proxy:1", false, func(s string) { set = append(set, s) })
	for _, answer := range []string{"org/proxy:1", "org/proxy:2", "org/proxy:2", "bad image;rm", "org/proxy:3"} {
		follow(answer)
	}
	if want := []string{"org/proxy:2", "org/proxy:3"}; strings.Join(set, ",") != strings.Join(want, ",") {
		t.Errorf("applied %v, want %v", set, want)
	}
	// A pinned runner's hook only warns: it never applies an answer.
	followOrgProxyImage("pinned/proxy:1", true, func(string) { t.Error("a pinned image was replaced") })("org/proxy:2")
}

// A pinned runner never takes the organisation's image, but it warns once for each distinct answer that
// names a different release; equal releases, development tags and an unpinned runner stay quiet.
func TestPinnedProxyImageWarnsOnSkew(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	warnings := func() int { return strings.Count(buf.String(), "level=WARN") }
	const old, current = "mirror.example/wardyn-proxy:0.9.3", "mirror.example/wardyn-proxy:0.9.5"
	set := func(string) { t.Error("a pinned runner took the organisation's image") }
	hook := followOrgProxyImage(old, true, set)
	if hook == nil {
		t.Fatal("a pinned runner has no hook to warn from")
	}
	hook(current)
	hook(current)
	if warnings() != 1 || !strings.Contains(buf.String(), old) || !strings.Contains(buf.String(), current) {
		t.Fatalf("0.9.3 against 0.9.5 warned %d times, want once naming both images:\n%s", warnings(), buf.String())
	}
	hook("mirror.example/wardyn-proxy:0.9.6")
	if warnings() != 2 {
		t.Fatalf("a second distinct answer warned %d times in all, want 2", warnings())
	}
	before := warnings()
	hook(old)
	followOrgProxyImage(old, true, set)("mirror.example/wardyn-proxy:dev")
	followOrgProxyImage("mirror.example/wardyn-proxy:latest", true, set)(current)
	followOrgProxyImage(old, true, set)("mirror.example/wardyn-proxy@sha256:" + strings.Repeat("a", 64))
	if warnings() != before {
		t.Fatalf("equal releases and non-release tags warned:\n%s", buf.String())
	}
	var took []string
	followOrgProxyImage(old, false, func(s string) { took = append(took, s) })(current)
	if len(took) != 1 || warnings() != before {
		t.Fatalf("an unpinned runner took %v and warned %d times", took, warnings()-before)
	}
}

func TestWarnPinnedProxyImageSkewAtStartup(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	for pinned, runner := range map[string]string{
		"m/p:0.9.5": "0.9.5", "m/p:dev": "0.9.5", "m/p:0.9.3": "0.9.5+abc123", "m/p": "0.9.5",
	} {
		warnPinnedProxyImageSkew(pinned, runner)
	}
	if buf.Len() != 0 {
		t.Fatalf("a matching, development or unversioned pin warned:\n%s", buf.String())
	}
	warnPinnedProxyImageSkew("m/p:0.9.3", "0.9.5")
	if !strings.Contains(buf.String(), "m/p:0.9.3") || !strings.Contains(buf.String(), "0.9.5") {
		t.Fatalf("no skew warning naming both:\n%s", buf.String())
	}
}
