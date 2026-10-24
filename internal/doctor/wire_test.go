// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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
		// Hijack before answering, and read through the buffer it returns: bytes the client sent behind
		// the CONNECT (or behind the 200) may already sit there.
		src, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = dst.Close()
			return
		}
		_, _ = io.WriteString(src, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { _, _ = io.Copy(dst, buf); _ = dst.Close() }()
		go func() { _, _ = io.Copy(src, dst); _ = src.Close() }()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The tunnel passes on bytes the client sends right behind its CONNECT: net/http may already have read them
// into the buffer Hijack hands back, which is how a ClientHello went missing on a loaded CI runner.
func TestTunnelKeepsBytesSentBehindTheConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		if c, err := ln.Accept(); err == nil {
			_, _ = io.Copy(c, c)
			_ = c.Close()
		}
	}()
	c, err := net.Dial("tcp", strings.TrimPrefix(tunnel(t), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	addr := ln.Addr().String()
	if _, err := io.WriteString(c, "CONNECT "+addr+" HTTP/1.1\r\nHost: "+addr+"\r\n\r\nping"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT answered %v, %v; want 200", res, err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
		t.Fatalf("echoed %q, %v; want the bytes sent behind the CONNECT", got, err)
	}
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

// A plain error at /healthz is still a failure of doctor's own, and doctor still skips the wire stage after it;
// only a command that asks to go on past it (setup) gets the wire stage too.
func TestPlainHealthzErrorFailsDoctorAndMaySkipTheWire(t *testing.T) {
	o := newOrg(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stream") {
			if c, err := websocket.Accept(w, r, nil); err == nil {
				_ = c.CloseNow()
			}
			return
		}
		http.NotFound(w, r)
	})
	n := runnerNet(o)
	stages := n.Run(context.Background())
	if h, w := byName(t, stages, "healthz"), byName(t, stages, "wire"); h.Status != StatusFail || !h.Advisory || w.Status != StatusSkip || (Report{Stages: stages}).OK() {
		t.Errorf("doctor: healthz = %s advisory %v, wire = %s, ok %v; want a failed advisory healthz, a skipped wire and a failed report", h.Status, h.Advisory, w.Status, Report{Stages: stages}.OK())
	}
	n = runnerNet(o)
	n.WireDespiteHealthz = true
	stages = n.Run(context.Background())
	if w := byName(t, stages, "wire"); w.Status != StatusOK {
		t.Errorf("with WireDespiteHealthz wire = %s %s; want ok", w.Status, w.Detail)
	}
	if _, blocked := (Report{Stages: stages}).FirstBlocking("healthz", "wire"); blocked {
		t.Error("an advisory healthz failure blocks")
	}
}
