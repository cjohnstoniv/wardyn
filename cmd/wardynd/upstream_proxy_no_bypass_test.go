// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestWarnUpstreamProxyNoBypass: a corp upstream proxy configured with no
// upstream_proxy_no_proxy entry covering a configured gateway host must warn
// at boot — every brokered call to that gateway is CONNECTed through the
// upstream instead of dialled directly, which times out on a private-endpoint
// estate the upstream cannot reach.
func TestWarnUpstreamProxyNoBypass(t *testing.T) {
	gateways := map[string]string{"api.anthropic.com": "https://gateway.internal:8443"}

	for name, c := range map[string]struct {
		st       pinPostureStore
		wantWarn bool
	}{
		"upstream configured, no covering entry": {
			st:       pinPostureStore{sc: types.SiteConfig{UpstreamProxyURL: "http://corp-proxy:8080"}},
			wantWarn: true,
		},
		"upstream configured, secret ref only, no covering entry": {
			st:       pinPostureStore{sc: types.SiteConfig{UpstreamProxySecretRef: "corp-proxy-url"}},
			wantWarn: true,
		},
		"upstream configured, covering suffix entry": {
			st: pinPostureStore{sc: types.SiteConfig{
				UpstreamProxyURL:     "http://corp-proxy:8080",
				UpstreamProxyNoProxy: []string{"gateway.internal"},
			}},
		},
		"no upstream proxy configured": {
			st: pinPostureStore{sc: types.SiteConfig{
				UpstreamProxyNoProxy: []string{"gateway.internal"},
			}},
		},
		"no site config at all": {
			st: pinPostureStore{},
		},
		"site config read failure is mute": {
			st: pinPostureStore{err: errors.New("pg down")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			warnUpstreamProxyNoBypass(context.Background(), c.st, gateways)

			warned := strings.Contains(buf.String(), "no upstream_proxy_no_proxy entry covers")
			if warned != c.wantWarn {
				t.Errorf("warned = %v, want %v; log = %s", warned, c.wantWarn, buf.String())
			}
		})
	}
}
