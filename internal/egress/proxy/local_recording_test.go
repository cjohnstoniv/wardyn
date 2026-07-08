// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// The brokered recording route: PUT /wardyn/v1/recordings/{runID} forwards to
// the control plane with the run token injected (the control plane enforces
// cross-run isolation: token run id must match the path run id). This is the
// default cast-delivery path — no shared volume, multi-node safe.
func TestLocalRecordingForwardsRunTokenAndBody(t *testing.T) {
	runID := uuid.New()
	var gotAuth, gotPath, gotBody string
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()

	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPut, routeRecordings+runID.String(), bytes.NewReader([]byte(`{"version":2}`)))
	req.Header.Set("Authorization", "Bearer SANDBOX-SMUGGLED")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer RUNTOK" {
		t.Fatalf("control plane Authorization = %q, want Bearer RUNTOK (sandbox header must be replaced)", gotAuth)
	}
	if gotPath != "/api/v1/internal/recordings/"+runID.String() {
		t.Fatalf("forwarded path = %q", gotPath)
	}
	if gotBody != `{"version":2}` {
		t.Fatalf("forwarded body = %q", gotBody)
	}
	if d := lastDecision(t, buf); d.RuleSource != ruleSourceRecordings || d.Decision != egress.Allow {
		t.Fatalf("decision = %+v, want brokered:recording allow", d)
	}
}

// Non-UUID run segments (incl. traversal shapes) never reach the forward.
func TestLocalRecordingRejectsNonUUID(t *testing.T) {
	cpCalled := false
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cpCalled = true }))
	defer cp.Close()
	p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)

	for _, bad := range []string{"..", "x", "../decisions", uuid.New().String() + "/extra"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodPut, routeRecordings+bad, bytes.NewReader([]byte("x"))))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%q: status = %d, want 404", bad, rec.Code)
		}
	}
	if cpCalled {
		t.Fatal("control plane must never be contacted for invalid run ids")
	}
}
