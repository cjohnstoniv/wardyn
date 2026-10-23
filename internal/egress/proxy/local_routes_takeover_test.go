// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestLocalTakeoverRaiseForwardsOnlyTheReason pins the "Needs you" raise (`wardyn needs-you`): the
// sandbox may shape nothing but the clamped reason. Tool fields it sends are dropped, the run token is
// the proxy's, and the forwarded body is exactly {kind, requested_scope:{reason}}.
func TestLocalTakeoverRaiseForwardsOnlyTheReason(t *testing.T) {
	apID := uuid.New()
	var gotAuth, gotBody string
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"`+apID.String()+`","state":"PENDING"}`)
	}))
	defer cp.Close()
	p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)

	long := strings.Repeat("y", types.MaxTakeoverReason+50)
	body, _ := json.Marshal(map[string]any{"kind": "human_takeover",
		"payload": map[string]any{"reason": "  " + long, "tool": "Bash", "cmd": "rm -rf /"}})
	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, routeApprovalsCreate, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer SANDBOX-SMUGGLED")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer RUNTOK" {
		t.Fatalf("control plane Authorization = %q, want the injected run token", gotAuth)
	}
	want := `{"kind":"human_takeover","requested_scope":{"reason":"` + strings.Repeat("y", types.MaxTakeoverReason) + `… (truncated)"}}`
	if gotBody != want {
		t.Fatalf("forwarded body = %q, want only the clamped reason", gotBody)
	}
}

// TestLocalTakeoverRaiseNeedsAReason: a takeover request with nothing to say never reaches the
// control plane.
func TestLocalTakeoverRaiseNeedsAReason(t *testing.T) {
	var contacted bool
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted = true
		w.WriteHeader(http.StatusCreated)
	}))
	defer cp.Close()
	p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)
	for _, b := range []string{`{"kind":"human_takeover","payload":{"reason":"   "}}`, `{"kind":"human_takeover"}`} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, routeApprovalsCreate, strings.NewReader(b)))
		if rec.Code != http.StatusBadRequest || contacted {
			t.Fatalf("%s: status = %d contacted=%v, want 400 before the control plane", b, rec.Code, contacted)
		}
	}
}
