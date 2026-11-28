// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reportProxy is a proxy whose control plane is cp.
func reportProxy(t *testing.T, cp *httptest.Server) *Proxy {
	t.Helper()
	return newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan queuedDecision, 64)},
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(cp)),
		ControlPlaneURL: "https://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		ControlTLS:      testInsecureTLSConfig,
	})
}

func shortReportBackoff(t *testing.T) {
	t.Helper()
	a, lo, hi := loadedReportAttempt, loadedReportMinBackoff, loadedReportMaxBackoff
	loadedReportAttempt, loadedReportMinBackoff, loadedReportMaxBackoff = 2*time.Second, 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { loadedReportAttempt, loadedReportMinBackoff, loadedReportMaxBackoff = a, lo, hi })
}

// The hash is the lowercase hex SHA-256 of the raw bytes.
func TestLoadedReport_ConfigSHA256(t *testing.T) {
	if got := ConfigSHA256([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("ConfigSHA256(abc) = %s", got)
	}
}

// A report whose first POST is dropped is sent again, with the run token, to the loaded route, and
// stops at the first 2xx; the body carries exactly config_sha256, proxy_version and switches.
func TestLoadedReport_RetriesAfterADroppedFirstPOST(t *testing.T) {
	shortReportBackoff(t)
	var (
		mu     sync.Mutex
		calls  int
		bodies []map[string]json.RawMessage
	)
	cp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/internal/proxy/loaded" || r.Header.Get("Authorization") != "Bearer RUNTOK" {
			t.Errorf("report %s %s auth %q; want POST /api/v1/internal/proxy/loaded with the run token", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if n == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Errorf("body %s: %v", b, err)
		}
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"state":"held"}`)
	}))
	defer cp.Close()
	p := reportProxy(t, cp)
	r := LoadedReport{ConfigSHA256: ConfigSHA256([]byte(`{"run_id":"x"}`)), ProxyVersion: "0.10.0", Switches: map[string]string{"WARDYN_LLM_SCAN": "off"}}
	done := make(chan struct{})
	go func() { p.reportLoaded(context.Background(), r, loadedReportTiming()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reporter never stopped after a 2xx")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 || len(bodies) != 1 {
		t.Fatalf("%d POSTs, %d read; want the dropped one and one retry", calls, len(bodies))
	}
	m := bodies[0]
	if len(m) != 3 || string(m["config_sha256"]) != `"`+r.ConfigSHA256+`"` || string(m["proxy_version"]) != `"0.10.0"` || string(m["switches"]) != `{"WARDYN_LLM_SCAN":"off","outage_pause":"on"}` {
		t.Fatalf("report body %v", m)
	}
}

// TestLoadedReport_CarriesOutagePause: every report says this proxy pauses and recovers (outage_pause "on"), with or
// without environment switches, and the caller's map is left as it was.
func TestLoadedReport_CarriesOutagePause(t *testing.T) {
	shortReportBackoff(t)
	got := make(chan map[string]string, 2)
	cp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Switches map[string]string `json:"switches"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode report: %v", err)
		}
		got <- body.Switches
		w.WriteHeader(http.StatusAccepted)
	}))
	defer cp.Close()
	p := reportProxy(t, cp)
	for _, sw := range []map[string]string{nil, {"WARDYN_MODEL_USAGE": "off"}} {
		p.reportLoaded(context.Background(), LoadedReport{ConfigSHA256: "x", ProxyVersion: "0.10.0", Switches: sw}, loadedReportTiming())
		posted := <-got
		if posted["outage_pause"] != "on" || len(posted) != len(sw)+1 {
			t.Fatalf("switches %v posted for %v; want them plus outage_pause on", posted, sw)
		}
		if _, leaked := sw["outage_pause"]; leaked {
			t.Fatal("the caller's switches map was changed")
		}
	}
}

// A control plane that never accepts the report does not keep a stopped proxy reporting.
func TestLoadedReport_StopsWithTheProxy(t *testing.T) {
	shortReportBackoff(t)
	cp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer cp.Close()
	p := reportProxy(t, cp)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.reportLoaded(ctx, LoadedReport{ConfigSHA256: "x"}, loadedReportTiming()); close(done) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the reporter stopped on a 503")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the reporter kept going after its proxy stopped")
	}
}

// An older wardynd answers 404 or 405 for the route: the reporter stops after one POST instead of
// retrying for the run's life.
func TestLoadedReport_StopsOnAnOlderControlPlane(t *testing.T) {
	shortReportBackoff(t)
	for _, code := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		var mu sync.Mutex
		posts := 0
		cp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			posts++
			mu.Unlock()
			w.WriteHeader(code)
		}))
		done := make(chan struct{})
		go func() {
			reportProxy(t, cp).reportLoaded(context.Background(), LoadedReport{ConfigSHA256: "x"}, loadedReportTiming())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("status %d: the reporter kept retrying", code)
		}
		cp.Close()
		mu.Lock()
		if posts != 1 {
			t.Fatalf("status %d: %d posts, want 1", code, posts)
		}
		mu.Unlock()
	}
}
