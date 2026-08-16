// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runLayoutMemStore is an in-memory store.RunLayoutStore fake, mirroring
// sshgateway_test.go's sshMemStore shape (embed store.Store for the unused
// rest of the surface, implement only what this lane's handlers call).
type runLayoutMemStore struct {
	store.Store
	rows map[string]types.RunLayout // key: runLayoutMemKey(principal, preset)
}

func newRunLayoutMemStore() *runLayoutMemStore {
	return &runLayoutMemStore{rows: map[string]types.RunLayout{}}
}

func runLayoutMemKey(principal, preset string) string { return principal + "\x00" + preset }

func (s *runLayoutMemStore) GetRunLayout(_ context.Context, principal, preset string) (types.RunLayout, error) {
	l, ok := s.rows[runLayoutMemKey(principal, preset)]
	if !ok {
		return types.RunLayout{}, store.ErrNotFound
	}
	return l, nil
}

func (s *runLayoutMemStore) PutRunLayout(_ context.Context, principal, preset string, layout []types.RunLayoutWidget) (types.RunLayout, error) {
	if layout == nil {
		layout = []types.RunLayoutWidget{}
	}
	l := types.RunLayout{Preset: preset, Layout: layout, UpdatedAt: time.Now().UTC()}
	s.rows[runLayoutMemKey(principal, preset)] = l
	return l, nil
}

// noRunLayoutStore satisfies store.Store (via the embedded interface) but
// deliberately implements neither GetRunLayout nor PutRunLayout — the "a
// store lacking the capability" case handlers must degrade for, per
// store.RunLayoutStore's doc.
type noRunLayoutStore struct{ store.Store }

// runLayoutTestServer builds a Server with OIDC configured (so ssoSession/
// doSSO can mint distinct principals for the cross-principal test) and a
// real in-memory RunLayoutStore wired.
func runLayoutTestServer(t *testing.T) (*Server, *runLayoutMemStore) {
	t.Helper()
	h := newHarness(t)
	st := newRunLayoutMemStore()
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg), st
}

func runLayoutDegradedTestServer(t *testing.T) *Server {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, noRunLayoutStore{})
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg)
}

type runLayoutBody struct {
	Preset    string                  `json:"preset"`
	Layout    []types.RunLayoutWidget `json:"layout"`
	UpdatedAt *time.Time              `json:"updated_at,omitempty"`
}

func decodeRunLayoutBody(t *testing.T, raw []byte) runLayoutBody {
	t.Helper()
	var b runLayoutBody
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("decode run layout body: %v; raw=%s", err, raw)
	}
	return b
}

// TestRunLayoutREST_PutThenGetRoundTrips is the core contract: a saved
// layout comes back exactly as saved, with a server-assigned updated_at.
func TestRunLayoutREST_PutThenGetRoundTrips(t *testing.T) {
	srv, _ := runLayoutTestServer(t)

	body := `{"preset":"live","layout":[{"widget":"terminal","x":0,"y":0,"w":8,"h":6},{"widget":"egress","x":8,"y":0,"w":4,"h":6}]}`
	w := do(t, srv, http.MethodPut, "/api/v1/me/run-layout", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("put: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	put := decodeRunLayoutBody(t, w.Body.Bytes())
	if put.Preset != "live" || len(put.Layout) != 2 || put.UpdatedAt == nil {
		t.Fatalf("put response = %+v, want preset=live, 2 widgets, a non-nil updated_at", put)
	}

	w = do(t, srv, http.MethodGet, "/api/v1/me/run-layout?preset=live", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeRunLayoutBody(t, w.Body.Bytes())
	if len(got.Layout) != 2 || got.Layout[0].Widget != "terminal" || got.Layout[1].Widget != "egress" {
		t.Errorf("get = %+v, want the two widgets just saved, in order", got)
	}
	if got.UpdatedAt == nil {
		t.Errorf("get after a save must carry updated_at")
	}
}

// TestRunLayoutREST_GetNoRowReturns200Empty pins the contract's central
// invariant: "I have never saved a layout" is a 200 with the empty shape,
// never a 404 — a 404 here would make a brand-new human's first cockpit
// load look like a failure.
func TestRunLayoutREST_GetNoRowReturns200Empty(t *testing.T) {
	srv, _ := runLayoutTestServer(t)

	w := do(t, srv, http.MethodGet, "/api/v1/me/run-layout?preset=finished", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeRunLayoutBody(t, w.Body.Bytes())
	if got.Preset != "finished" || len(got.Layout) != 0 || got.UpdatedAt != nil {
		t.Errorf("got = %+v, want preset echoed back, an EMPTY (not null) layout, no updated_at", got)
	}
	if body := w.Body.String(); !json.Valid([]byte(body)) {
		t.Fatalf("response is not valid JSON: %s", body)
	}
}

// TestRunLayoutREST_InvalidPresetIs400 covers both verbs: preset is a closed
// set, and an open string must never reach the store (unbounded row-per-typo).
func TestRunLayoutREST_InvalidPresetIs400(t *testing.T) {
	srv, _ := runLayoutTestServer(t)

	if w := do(t, srv, http.MethodGet, "/api/v1/me/run-layout?preset=archived", adminToken, ""); w.Code != http.StatusBadRequest {
		t.Errorf("get bad preset: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if w := do(t, srv, http.MethodGet, "/api/v1/me/run-layout", adminToken, ""); w.Code != http.StatusBadRequest {
		t.Errorf("get missing preset: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}

	body := `{"preset":"archived","layout":[]}`
	if w := do(t, srv, http.MethodPut, "/api/v1/me/run-layout", adminToken, body); w.Code != http.StatusBadRequest {
		t.Errorf("put bad preset: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestRunLayoutREST_UnknownWidgetIDRejected pins the "reject, don't store"
// rule: an id no build renders must never make it into the row.
func TestRunLayoutREST_UnknownWidgetIDRejected(t *testing.T) {
	srv, st := runLayoutTestServer(t)

	body := `{"preset":"live","layout":[{"widget":"not-a-real-widget","x":0,"y":0,"w":1,"h":1}]}`
	w := do(t, srv, http.MethodPut, "/api/v1/me/run-layout", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if len(st.rows) != 0 {
		t.Errorf("a rejected widget id must not be stored, rows = %+v", st.rows)
	}
}

// TestRunLayoutREST_ScopedToOwnPrincipal is the security invariant the
// contract calls out explicitly: a layout saved by principal A must not be
// visible to principal B.
func TestRunLayoutREST_ScopedToOwnPrincipal(t *testing.T) {
	srv, _ := runLayoutTestServer(t)
	alice := ssoSession(t, "alice-sub", "alice@example.com", oidc.RoleMember)
	bob := ssoSession(t, "bob-sub", "bob@example.com", oidc.RoleMember)

	body := `{"preset":"live","layout":[{"widget":"identity","x":0,"y":0,"w":12,"h":4}]}`
	w := doSSO(t, srv, http.MethodPut, "/api/v1/me/run-layout", alice, body)
	if w.Code != http.StatusOK {
		t.Fatalf("alice's put: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	w = doSSO(t, srv, http.MethodGet, "/api/v1/me/run-layout?preset=live", bob, "")
	if w.Code != http.StatusOK {
		t.Fatalf("bob's get: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeRunLayoutBody(t, w.Body.Bytes())
	if len(got.Layout) != 0 {
		t.Errorf("bob's get = %+v, want the empty default (alice's layout must not leak)", got)
	}
}

// TestRunLayoutREST_DegradesWithoutCapability pins store.RunLayoutStore's
// documented fallback: GET returns the empty shape at 200, PUT 501s, when
// s.cfg.Store does not implement the capability interface.
func TestRunLayoutREST_DegradesWithoutCapability(t *testing.T) {
	srv := runLayoutDegradedTestServer(t)

	w := do(t, srv, http.MethodGet, "/api/v1/me/run-layout?preset=live", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeRunLayoutBody(t, w.Body.Bytes())
	if len(got.Layout) != 0 {
		t.Errorf("get without a RunLayoutStore = %+v, want the empty default", got)
	}

	body := `{"preset":"live","layout":[]}`
	w = do(t, srv, http.MethodPut, "/api/v1/me/run-layout", adminToken, body)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("put: code = %d, want 501; body=%s", w.Code, w.Body.String())
	}
}
