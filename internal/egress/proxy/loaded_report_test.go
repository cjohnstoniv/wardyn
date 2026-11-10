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

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reportProxy is a proxy whose control plane is cp.
func reportProxy(t *testing.T, cp *httptest.Server) *Proxy {
	t.Helper()
	return newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
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
	go func() { p.reportLoaded(context.Background(), r); close(done) }()
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
	if len(m) != 3 || string(m["config_sha256"]) != `"`+r.ConfigSHA256+`"` || string(m["proxy_version"]) != `"0.10.0"` || string(m["switches"]) != `{"WARDYN_LLM_SCAN":"off"}` {
		t.Fatalf("report body %v", m)
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
	go func() { p.reportLoaded(ctx, LoadedReport{ConfigSHA256: "x"}); close(done) }()
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
