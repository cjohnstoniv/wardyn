// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Strict audit (audit_strict.go): with strict mode on, a covered change is refused if its audit intent
// row cannot be written first; the handler's own detailed row is unchanged and can still spool.

// strictRecorder stands in for the strict recorder: it keeps what it took, or fails every write with err.
type strictRecorder struct {
	mu     sync.Mutex
	events []types.AuditEvent
	err    error
}

func (r *strictRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, ev)
	return nil
}

func (r *strictRecorder) snapshot() []types.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]types.AuditEvent(nil), r.events...)
}

// spoolOnlyRecorder is the serving chain with its store down: every row fails the store write and is held
// in the spool, which is what audit.ErrSpooled says (cmd/wardynd spoolingRecorder).
type spoolOnlyRecorder struct {
	mu      sync.Mutex
	spooled []types.AuditEvent
}

func (r *spoolOnlyRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spooled = append(r.spooled, ev)
	return fmt.Errorf("%w: the store is down", audit.ErrSpooled)
}

const strictWallsBody = `{"spec":{"allowed_domains":["pypi.org"]}}`

// TestAuditStrictIntentFailureRefusesTheChange: when the intent row cannot be written, the change is
// refused 503 audit_unavailable, its handler never runs and the store is unchanged.
func TestAuditStrictIntentFailureRefusesTheChange(t *testing.T) {
	srv, st, h := uvServer(t)
	strict := &strictRecorder{err: errors.New("the store is down")}
	srv.cfg.AuditStrict = strict
	member := ssoSession(t, wallsSub, "walls@corp.example", oidc.RoleUser)

	w := doSSO(t, srv, http.MethodPut, "/api/v1/me/walls", member, strictWallsBody)
	if w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonAuditUnavailable {
		t.Fatalf("PUT /me/walls with the intent write failing = %d %s, want 503 %s", w.Code, w.Body, reasonAuditUnavailable)
	}
	if v, ok := st.prefs[wallsSub+"|"+types.PersonalWallsPrefKey]; ok {
		t.Errorf("the refused change was stored: %s", v)
	}
	if row := wallsAuditRow(h.audit.snapshot(), auditWallsSet); row != nil {
		t.Errorf("the handler ran and wrote %s: %v", auditWallsSet, row)
	}
	if got := strict.snapshot(); len(got) != 0 {
		t.Errorf("the failing strict recorder kept %v", got)
	}
}

// TestAuditStrictLimitDetailedRowCanSpool pins the stated limit: the intent row is written, the
// handler's own row fails the store and spools, and the change still commits.
func TestAuditStrictLimitDetailedRowCanSpool(t *testing.T) {
	srv, st, _ := uvServer(t)
	strict := &strictRecorder{}
	spool := &spoolOnlyRecorder{}
	srv.cfg.AuditStrict, srv.cfg.Audit = strict, spool
	member := ssoSession(t, wallsSub, "walls@corp.example", oidc.RoleUser)

	if w := doSSO(t, srv, http.MethodPut, "/api/v1/me/walls", member, strictWallsBody); w.Code != http.StatusOK {
		t.Fatalf("PUT /me/walls = %d %s, want 200", w.Code, w.Body)
	}
	var stored types.PersonalWalls
	if err := json.Unmarshal(st.prefs[wallsSub+"|"+types.PersonalWallsPrefKey], &stored); err != nil ||
		stored.Spec.AllowedDomains == nil || len(*stored.Spec.AllowedDomains) != 1 {
		t.Fatalf("stored walls = %+v (%v): the body did not reach the handler after the intent row was written", stored, err)
	}
	if wallsAuditRow(spool.spooled, auditWallsSet) == nil {
		t.Errorf("the handler's %s row was not handed to the spool: %v", auditWallsSet, spool.spooled)
	}
	intents := strict.snapshot()
	if len(intents) != 1 || intents[0].Action != "audit.intent.record" || intents[0].Outcome != "success" || intents[0].Actor != wallsSub {
		t.Fatalf("strict rows = %v, want one audit.intent.record by the caller", intents)
	}
	var data map[string]any
	if err := json.Unmarshal(intents[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(strictWallsBody))
	rid, _ := data["request_id"].(string)
	if data["method"] != http.MethodPut || data["path"] != "/api/v1/me/walls" || data["route"] != "/api/v1/me/walls" ||
		data["body_sha256"] != hex.EncodeToString(sum[:]) || rid == "" {
		t.Errorf("intent data = %s", intents[0].Data)
	}
	for _, ev := range spool.spooled {
		if ev.Action == "audit.intent.record" {
			t.Errorf("the intent row went to the spooling recorder: %v", ev)
		}
	}
}

// TestAuditStrictIntentNamesTheTarget: the intent row carries the resolved path, the route pattern and
// the path's ids.
func TestAuditStrictIntentNamesTheTarget(t *testing.T) {
	srv, _, _, _ := newAuthzMatrixServer(t)
	strict := &strictRecorder{}
	srv.cfg.AuditStrict = strict
	admin := ssoSession(t, "strict-admin", "strict-admin@corp.example", oidc.RoleAdmin)
	id := uuid.New()
	_ = doSSO(t, srv, http.MethodDelete, "/api/v1/policies/"+id.String(), admin, "")
	intents := strict.snapshot()
	if len(intents) != 1 || intents[0].Target != "/api/v1/policies/"+id.String() {
		t.Fatalf("strict rows = %v, want one naming /api/v1/policies/%s", intents, id)
	}
	var data struct {
		Route  string            `json:"route"`
		Params map[string]string `json:"params"`
	}
	if err := json.Unmarshal(intents[0].Data, &data); err != nil || data.Route != "/api/v1/policies/{id}" || data.Params["id"] != id.String() || len(data.Params) != 1 {
		t.Errorf("intent data = %s (%v), want route /api/v1/policies/{id} and params {id}", intents[0].Data, err)
	}
}

// TestAuditStrictLeavesTheRestAlone: an excluded read-only POST and a read are not held by a failing
// intent write, and with strict mode off a covered change writes no intent row.
func TestAuditStrictLeavesTheRestAlone(t *testing.T) {
	srv, _, _ := uvServer(t)
	srv.cfg.AuditStrict = &strictRecorder{err: errors.New("the store is down")}
	member := ssoSession(t, wallsSub, "walls@corp.example", oidc.RoleUser)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/policies/grade", `{}`},
		{http.MethodGet, "/api/v1/me/walls", ""},
	} {
		if w := doSSO(t, srv, c.method, c.path, member, c.body); w.Code == http.StatusServiceUnavailable {
			t.Errorf("%s %s = 503 %s; it is not a covered change", c.method, c.path, w.Body)
		}
	}

	off, _, _ := uvServer(t)
	if w := doSSO(t, off, http.MethodPut, "/api/v1/me/walls", member, strictWallsBody); w.Code != http.StatusOK {
		t.Errorf("PUT /me/walls with strict off = %d %s, want 200", w.Code, w.Body)
	}
}

// TestAuditStrictIntentNamesTheWildcardTarget: a capability availability change names its target in the
// route's wildcard (an image ref, slashes and all), and the intent row carries it.
func TestAuditStrictIntentNamesTheWildcardTarget(t *testing.T) {
	srv, _, _, _ := newAuthzMatrixServer(t)
	strict := &strictRecorder{}
	srv.cfg.AuditStrict = strict
	admin := ssoSession(t, "strict-admin", "strict-admin@corp.example", oidc.RoleAdmin)
	_ = doSSO(t, srv, http.MethodPut, "/api/v1/permissions/availability/image/ghcr.io/acme/tool:1", admin, `{}`)
	intents := strict.snapshot()
	if len(intents) != 1 {
		t.Fatalf("strict rows = %v, want one", intents)
	}
	var data struct {
		Route  string            `json:"route"`
		Params map[string]string `json:"params"`
	}
	if err := json.Unmarshal(intents[0].Data, &data); err != nil || data.Route != "/api/v1/permissions/availability/{kind}/*" ||
		data.Params["kind"] != "image" || data.Params["*"] != "ghcr.io/acme/tool:1" {
		t.Errorf("intent data = %s (%v), want the availability route with kind image and * the image ref", intents[0].Data, err)
	}
}

// TestAuditStrictIntentCapsTheCallersURL: a member with no role on the route sends a covered change with a
// 100 KiB id and a 100 KiB request id. The intent row is written before the role gate, so each value the
// caller chose is cut to the audit target's cap; the row still names the route and the body's hash.
func TestAuditStrictIntentCapsTheCallersURL(t *testing.T) {
	srv, _, _, _ := newAuthzMatrixServer(t)
	strict := &strictRecorder{}
	srv.cfg.AuditStrict = strict
	member := ssoSession(t, wallsSub, "walls@corp.example", oidc.RoleUser)
	long := strings.Repeat("a", 100<<10)
	r := httptest.NewRequest(http.MethodPut, "/api/v1/policies/"+long, strings.NewReader(`{}`))
	r.Header.Set(middleware.RequestIDHeader, long)
	r.AddCookie(member)
	panicFails(t, srv.Handler()).ServeHTTP(httptest.NewRecorder(), r)

	intents := strict.snapshot()
	if len(intents) != 1 {
		t.Fatalf("strict rows = %d, want one", len(intents))
	}
	if n := len(intents[0].Data) + len(intents[0].Target); n > 4<<10 {
		t.Errorf("the intent row carries %d bytes of the caller's URL, want the capped few", n)
	}
	var data map[string]any
	if err := json.Unmarshal(intents[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(`{}`))
	if data["route"] != "/api/v1/policies/{id}" || data["body_sha256"] != hex.EncodeToString(sum[:]) {
		t.Errorf("intent data lost the route or the body hash: route %v, body_sha256 %v", data["route"], data["body_sha256"])
	}
}
