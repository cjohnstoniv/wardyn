// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// wireOrg is the organisation as a runner meets it: /healthz, and a stream route that accepts a WebSocket the
// way the real one does (websocket.Accept), recording what it was asked.
func wireOrg(t *testing.T, wrap func(http.Handler) http.Handler) (org, *[]*http.Request) {
	t.Helper()
	var mu sync.Mutex
	var seen []*http.Request
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/stream") {
			healthy(w, r)
			return
		}
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
		if c, err := websocket.Accept(w, r, nil); err == nil {
			_ = c.CloseNow()
		}
	})
	if wrap != nil {
		h = wrap(h)
	}
	return newOrg(t, h.ServeHTTP), &seen
}

// stripUpgrade is a gateway that drops the hop-by-hop headers a WebSocket upgrade needs.
func stripUpgrade(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("Connection")
		r.Header.Del("Upgrade")
		next.ServeHTTP(w, r)
	})
}

// tunnel is a CONNECT proxy that passes bytes through and nothing else.
func tunnel(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dst, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		src, _, _ := w.(http.Hijacker).Hijack()
		go func() { _, _ = io.Copy(dst, src); _ = dst.Close() }()
		go func() { _, _ = io.Copy(src, dst); _ = src.Close() }()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func runnerNet(o org) Network {
	n := base(o)
	n.Identity, n.Token, n.SystemRoots, n.OrgSetting, n.Wire = false, "", o.pool, "WARDYN_RUNNER_ORG_URL", true
	return n
}

func TestWireStageNamesWhatBlocksTheUpgrade(t *testing.T) {
	stripped, _ := wireOrg(t, stripUpgrade)
	portal := newOrg(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stream") {
			_, _ = w.Write([]byte("<html>welcome</html>"))
			return
		}
		healthy(w, r)
	})
	off := newOrg(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stream") {
			http.NotFound(w, r)
			return
		}
		healthy(w, r)
	})
	for _, tc := range []struct {
		name    string
		net     func() Network
		setting string
		in      string
	}{
		{"a gateway strips the upgrade", func() Network { return runnerNet(stripped) }, "WARDYN_RUNNER_ORG_URL", "answered 426 instead of switching to a WebSocket"},
		{"a proxy in HTTPS_PROXY lets the stripped upgrade reach the organisation", func() Network {
			n := runnerNet(stripped)
			n.ProxyFor = viaProxy(t, tunnel(t))
			return n
		}, "HTTPS_PROXY", "the proxy 127.0.0.1:"},
		{"something answers the route with a page", func() Network { return runnerNet(portal) }, "WARDYN_RUNNER_ORG_URL", "answered 200 instead of switching"},
		{"the route is not served", func() Network { return runnerNet(off) }, "WARDYN_RUNNER_ORG_URL", "answered 404: runners are switched off there"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := tc.net()
			stages := n.Run(context.Background())
			s := byName(t, stages, "wire")
			if s.Status != StatusFail || s.Setting != tc.setting || !strings.Contains(s.Detail, tc.in) || !strings.Contains(s.Fix, "set "+tc.setting+" to") {
				t.Errorf("wire = %s setting %q detail %q fix %q, want FAIL naming %s and %q\n%s", s.Status, s.Setting, s.Detail, s.Fix, tc.setting, tc.in, text(t, stages))
			}
		})
	}
}

func TestWireStagePassesAnUpgradeThatGetsThrough(t *testing.T) {
	o, seen := wireOrg(t, nil)
	n := runnerNet(o)
	n.Token = "tok-secret-1234"
	stages := n.Run(context.Background())
	if s := byName(t, stages, "wire"); s.Status != StatusOK {
		t.Fatalf("wire = %s %q, want ok\n%s", s.Status, s.Detail, text(t, stages))
	}
	if len(*seen) != 1 {
		t.Fatalf("the stream route saw %d requests, want 1", len(*seen))
	}
	// It asks as nobody: the nil runner id, and no credential of the runner's.
	r := (*seen)[0]
	if !strings.Contains(r.URL.Path, "/00000000-0000-0000-0000-000000000000/") || r.Header.Get("Authorization") != "" {
		t.Errorf("the probe asked %s with Authorization %q, want the nil runner and no credential", r.URL.Path, r.Header.Get("Authorization"))
	}
}

func TestWireStageIsSkippedWhenTheOrganisationDoesNotAnswer(t *testing.T) {
	o, seen := wireOrg(t, nil)
	n := runnerNet(o)
	n.SystemRoots = nil // the handshake fails, so /healthz is never reached
	stages := n.Run(context.Background())
	if s := byName(t, stages, "wire"); s.Status != StatusSkip || len(*seen) != 0 {
		t.Errorf("wire = %s, stream requests %d; want skip and none", s.Status, len(*seen))
	}
}

func TestNoWireStageUnlessAsked(t *testing.T) {
	o, seen := wireOrg(t, nil)
	n := runnerNet(o)
	n.Wire = false
	for _, s := range n.Run(context.Background()) {
		if s.Name == "wire" {
			t.Errorf("wire stage without Wire: %+v", s)
		}
	}
	if len(*seen) != 0 {
		t.Errorf("the stream route was asked %d times", len(*seen))
	}
}
