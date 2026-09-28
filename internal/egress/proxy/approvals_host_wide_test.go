// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// P0.3 — an egress_domain approval is HOST-WIDE,
// on every port, and 0.7.2's decision is to keep it that way and make the
// surfaces SAY so rather than leave it resting on a caller's convention one call
// site away. The convention is real (evaluate's splitHostPort removes the port
// before Resolve ever sees the host) but nothing here enforced it, and the two
// surfaces that would disagree if it broke — the cache key and the raised
// requested_scope a human READS — are both spelled from the same string.

// raiseRecorder is a control plane that records the requested_scope of every
// raise, so a test can read the bytes a human would be shown.
type raiseRecorder struct {
	mu     sync.Mutex
	scopes []egressScope
	raises int
}

func (rr *raiseRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var body raiseBody
			_ = json.NewDecoder(r.Body).Decode(&body)
			rr.mu.Lock()
			rr.raises++
			rr.scopes = append(rr.scopes, body.RequestedScope)
			rr.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
		default:
			// A poll of a still-PENDING approval.
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (rr *raiseRecorder) count() int {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return rr.raises
}

// TestApprovalsAreHostWide_OnePortDoesNotOpenASecondRow: asking about two ports
// of one host is ONE approval and ONE question in front of a human. Were the key
// ever port-bearing, a run touching :443 and :8443 would raise two rows for one
// decision an operator thinks they already made — and the grant they made would
// then be narrower than the docs, the scope table and the console all say.
func TestApprovalsAreHostWide_OnePortDoesNotOpenASecondRow(t *testing.T) {
	rr := &raiseRecorder{}
	cp := rr.server(t)
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())

	for _, host := range []string{"pkg.example.com", "pkg.example.com:443", "pkg.example.com:8443", "PKG.example.com.", "pkg.example.com"} {
		ap.Resolve(context.Background(), host)
	}

	if got := rr.count(); got != 1 {
		t.Fatalf("raises = %d for five spellings of ONE host, want 1 — an egress_domain approval is host-wide, "+
			"so a port (or a case, or a trailing dot) must not open a second row", got)
	}
	ap.mu.Lock()
	keys := make([]string, 0, len(ap.hosts))
	for k := range ap.hosts {
		keys = append(keys, k)
	}
	ap.mu.Unlock()
	if len(keys) != 1 || keys[0] != "pkg.example.com" {
		t.Errorf("cache keys = %v, want exactly [pkg.example.com] — the key is the bare host", keys)
	}
}

// TestApprovalsAreHostWide_RaisedScopeCarriesNoPort: the other surface. The
// requested_scope is stored verbatim by the control plane and rendered on the
// approval card, so if a port ever reached it the human would be shown a
// narrowness the grant does not have (and the durable always-write would then be
// refused by hostrules.ValidApprovedHost, which takes no port by construction).
func TestApprovalsAreHostWide_RaisedScopeCarriesNoPort(t *testing.T) {
	rr := &raiseRecorder{}
	cp := rr.server(t)
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())

	ap.Resolve(context.Background(), "registry.example.com:5000")

	rr.mu.Lock()
	defer rr.mu.Unlock()
	if len(rr.scopes) != 1 {
		t.Fatalf("scopes recorded = %d, want 1", len(rr.scopes))
	}
	if got := rr.scopes[0].Host; got != "registry.example.com" {
		t.Errorf("raised requested_scope.host = %q, want the bare host — the scope a human reads must not imply "+
			"a port the grant does not honour", got)
	}
}

// TestApprovalHostKey is the unit half: the normalization is one function so the
// key and the raised scope can never be spelled differently.
func TestApprovalHostKey(t *testing.T) {
	for in, want := range map[string]string{
		"example.com":       "example.com",
		"example.com:443":   "example.com",
		"EXAMPLE.com":       "example.com",
		"example.com.":      "example.com",
		"example.com..:443": "example.com",
		"[::1]:8080":        "::1",
		"":                  "",
	} {
		if got := approvalHostKey(in); got != want {
			t.Errorf("approvalHostKey(%q) = %q, want %q", in, got, want)
		}
	}
}
