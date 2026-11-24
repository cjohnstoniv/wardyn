// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// assistantCP is a control-plane fake that records every call reaching it.
type assistantCP struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []assistantCPCall
}

type assistantCPCall struct {
	method, uri, auth string
	body              []byte
}

func newAssistantCP(t *testing.T) *assistantCP {
	t.Helper()
	cp := &assistantCP{}
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cp.mu.Lock()
		cp.calls = append(cp.calls, assistantCPCall{method: r.Method, uri: r.URL.RequestURI(), auth: r.Header.Get("Authorization"), body: b})
		cp.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answer":"from the control plane"}`)
	}))
	t.Cleanup(cp.srv.Close)
	return cp
}

func (cp *assistantCP) taken() []assistantCPCall {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]assistantCPCall(nil), cp.calls...)
}

// assistantCall sends one sandbox call to a fresh proxy and answers the response, what reached the control
// plane, and the decision log.
func assistantCall(t *testing.T, method, path string, body []byte) (*httptest.ResponseRecorder, []assistantCPCall, *bytes.Buffer) {
	t.Helper()
	cp := newAssistantCP(t)
	p, decisions := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp.srv), nil, nil)
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := mustLocalReq(t, method, path, rdr)
	req.Header.Set("Authorization", "Bearer SANDBOX-SMUGGLED")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	return w, cp.taken(), decisions
}

// TestAssistantRelayToolName: the path segment after tools/ must match ^[a-z_]{1,32}$.
func TestAssistantRelayToolName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"read", true}, {"a_b", true}, {strings.Repeat("a", 32), true},
		{strings.Repeat("a", 33), false}, {"", false}, {"Read", false}, {"a-b", false}, {"a1", false},
		{"..", false}, {"a/b", false}, {"a%2Fb", false}, {"read/", false},
	} {
		t.Run("name="+tc.name, func(t *testing.T) {
			w, calls, _ := assistantCall(t, http.MethodPost, routeAssistant+"tools/"+tc.name, []byte(`{"session_id":"s","args":{}}`))
			if tc.ok {
				if w.Code != http.StatusOK || len(calls) != 1 || calls[0].uri != "/api/v1/internal/assistant/tools/"+tc.name {
					t.Fatalf("status %d, calls %+v; want it forwarded to the tool's route", w.Code, calls)
				}
				return
			}
			if w.Code != http.StatusBadRequest || len(calls) != 0 {
				t.Fatalf("status %d, %d control-plane calls; want 400 with nothing forwarded", w.Code, len(calls))
			}
		})
	}
}

// TestAssistantRelayMethods: four method and path pairs pass; every other method is 405.
func TestAssistantRelayMethods(t *testing.T) {
	for _, tc := range []struct {
		method, path, wantURI string
	}{
		{http.MethodGet, "turns", "/api/v1/internal/assistant/turns?after=0"},
		{http.MethodGet, "tools", "/api/v1/internal/assistant/tools"},
		{http.MethodPost, "events", "/api/v1/internal/assistant/events"},
		{http.MethodPost, "tools/read", "/api/v1/internal/assistant/tools/read"},
		{http.MethodPost, "turns", ""},
		{http.MethodGet, "events", ""},
		{http.MethodPut, "tools/read", ""},
		{http.MethodDelete, "tools", ""},
		{http.MethodGet, "tools/read", ""},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var body []byte
			if tc.method != http.MethodGet {
				body = []byte(`{"session_id":"s"}`)
			}
			w, calls, decisions := assistantCall(t, tc.method, routeAssistant+tc.path, body)
			if tc.wantURI == "" {
				if w.Code != http.StatusMethodNotAllowed || len(calls) != 0 {
					t.Fatalf("status %d, %d control-plane calls; want 405 with nothing forwarded", w.Code, len(calls))
				}
				return
			}
			if w.Code != http.StatusOK || w.Body.String() != `{"answer":"from the control plane"}` || len(calls) != 1 {
				t.Fatalf("status %d body %q calls %d; want the control plane's answer passed through", w.Code, w.Body.String(), len(calls))
			}
			c := calls[0]
			if c.method != tc.method || c.uri != tc.wantURI || c.auth != "Bearer RUNTOK" || !bytes.Equal(c.body, body) {
				t.Fatalf("forwarded %+v; want %s %s with the run token and the body as sent", c, tc.method, tc.wantURI)
			}
			if d := lastDecision(t, decisions); d.Decision != egress.Allow || d.RuleSource != ruleSourceAssistant {
				t.Fatalf("decision %+v; want an allow under %s", d, ruleSourceAssistant)
			}
		})
	}
	w, calls, _ := assistantCall(t, http.MethodGet, routeAssistant+"other", nil)
	if w.Code != http.StatusNotFound || len(calls) != 0 {
		t.Fatalf("an unknown assistant path: status %d, %d calls", w.Code, len(calls))
	}
}

// TestAssistantRelayBodyCap: 256 KiB is forwarded whole; one byte more is refused with 413 and a deny.
func TestAssistantRelayBodyCap(t *testing.T) {
	for _, path := range []string{"events", "tools/read"} {
		at := bytes.Repeat([]byte("x"), maxAssistantBody)
		w, calls, _ := assistantCall(t, http.MethodPost, routeAssistant+path, at)
		if maxAssistantBody != 262144 || w.Code != http.StatusOK || len(calls) != 1 || len(calls[0].body) != 262144 {
			t.Fatalf("%s at the cap: status %d, calls %d; want it forwarded whole", path, w.Code, len(calls))
		}
		w, calls, decisions := assistantCall(t, http.MethodPost, routeAssistant+path, append(at, 'x'))
		if w.Code != http.StatusRequestEntityTooLarge || len(calls) != 0 {
			t.Fatalf("%s over the cap: status %d, calls %d; want 413 with nothing forwarded", path, w.Code, len(calls))
		}
		if d := lastDecision(t, decisions); d.Decision != egress.Deny || d.RuleSource != ruleSourceAssistant {
			t.Fatalf("%s over the cap: decision %+v", path, d)
		}
	}
}

// TestAssistantRelayCursor: GET turns forwards only the parsed after number; a bad cursor is 400 and is not
// forwarded; every other parameter is dropped.
func TestAssistantRelayCursor(t *testing.T) {
	for _, tc := range []struct {
		query, want string
	}{
		{"after=7", "after=7"},
		{"", "after=0"},
		{"after=007", "after=7"},
		{"after=7&x=1", "after=7"},
		{"x=1", "after=0"},
		{"after=", ""},
		{"after=+7", ""},
		{"after=-1", ""},
		{"after=abc", ""},
		{"after=1e3", ""},
		{"after=7;x", ""},
		{"after=1&after=2", ""},
		{"after=9223372036854775808", ""},
	} {
		t.Run("query="+tc.query, func(t *testing.T) {
			path := routeAssistant + "turns"
			if tc.query != "" {
				path += "?" + tc.query
			}
			w, calls, _ := assistantCall(t, http.MethodGet, path, nil)
			if tc.want == "" {
				if w.Code != http.StatusBadRequest || len(calls) != 0 {
					t.Fatalf("status %d, %d calls; want 400 with nothing forwarded", w.Code, len(calls))
				}
				return
			}
			if w.Code != http.StatusOK || len(calls) != 1 || calls[0].uri != "/api/v1/internal/assistant/turns?"+tc.want {
				t.Fatalf("status %d, calls %+v; want the control plane to see %s", w.Code, calls, tc.want)
			}
		})
	}
	for _, path := range []string{"tools?after=3&x=1", "events?after=3"} {
		method := http.MethodGet
		var body []byte
		if strings.HasPrefix(path, "events") {
			method, body = http.MethodPost, []byte(`{}`)
		}
		_, calls, _ := assistantCall(t, method, routeAssistant+path, body)
		if len(calls) != 1 || strings.Contains(calls[0].uri, "?") {
			t.Fatalf("%s: calls %+v; want the query dropped", path, calls)
		}
	}
}

// TestAssistantRelayControlPlaneDown: the route fails closed like every brokered route (the table in
// local_routes_test.go reads only handlers defined in local_routes.go): 502 and a deny under its rule source.
func TestAssistantRelayControlPlaneDown(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "turns?after=1"}, {http.MethodGet, "tools"}, {http.MethodPost, "events"}, {http.MethodPost, "tools/read"},
	} {
		p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", "127.0.0.1:1", nil, nil)
		var body io.Reader
		if tc.method == http.MethodPost {
			body = strings.NewReader(`{}`)
		}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, mustLocalReq(t, tc.method, routeAssistant+tc.path, body))
		if w.Code != http.StatusBadGateway {
			t.Fatalf("%s %s: status %d, want 502", tc.method, tc.path, w.Code)
		}
		if d := lastDecision(t, buf); d.Decision != egress.Deny || d.RuleSource != ruleSourceAssistant {
			t.Fatalf("%s %s: decision %+v", tc.method, tc.path, d)
		}
	}
}
