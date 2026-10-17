// Copyright 2026 The Wardyn Authors
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
