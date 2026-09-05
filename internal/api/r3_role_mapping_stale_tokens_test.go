// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// roleMapTokenStore is roleMapStore plus the api_tokens listing the stale-
// snapshot count reads.
type roleMapTokenStore struct {
	roleMapStore
	toks []types.APIToken
}

func (s *roleMapTokenStore) ListAPITokens(context.Context) ([]types.APIToken, error) {
	return s.toks, nil
}

// TestRoleMappingWriteReportsStaleTokenSnapshots is F112.
//
// An api_token's role is stamped at MINT and read verbatim on every request;
// since 0.7 that stamp can be security_admin. The sibling credential got a bound
// in migration 0046 — an SSH key's admin override goes stale after
// WARDYN_SSH_ROLE_TTL — and a wdn_ token got none, so removing someone's admin
// through the People screen left their outstanding tokens holding it until a
// human separately remembered DELETE /tokens/{id} or POST /sessions/revoke.
// Nothing in the demotion path prompted either.
//
// This is the "at minimum" remedy the finding names, and NOT an auto-revoke: a
// mapping value is a GROUP key, so revoking on edit would let one People-screen
// change kill every CI credential whose frozen snapshot happens to name that
// group. The admin is told, in the two places they look — the response and the
// audit row — plus a WARN for the operator who is not looking.
func TestRoleMappingWriteReportsStaleTokenSnapshots(t *testing.T) {
	const group = "eng-team"
	now := time.Now().UTC()
	revoked := now.Add(-time.Hour)
	toks := []types.APIToken{
		// Bound by the GROUP snapshot — the case a mapping edit cannot reach.
		{ID: uuid.New(), Principal: "sub-alice", Email: "alice@corp.example", Role: string(oidc.RoleAdmin), Groups: []string{group}, CreatedAt: now},
		// Bound by EMAIL, for a deployment that opted into email-keyed rows.
		{ID: uuid.New(), Principal: "sub-bob", Email: group + "@corp.example", Role: string(oidc.RoleMember), CreatedAt: now},
		// Already revoked: not a live credential, must not be counted.
		{ID: uuid.New(), Principal: "sub-carol", Groups: []string{group}, RevokedAt: &revoked, CreatedAt: now},
		// A different group entirely: the count must be about THIS value.
		{ID: uuid.New(), Principal: "sub-dave", Groups: []string{"other-team"}, CreatedAt: now},
	}

	newSrv := func(t *testing.T) (*Server, *roleMapTokenStore) {
		t.Helper()
		st := &roleMapTokenStore{toks: toks}
		auth := newAccessAuth(t, map[string]string{"chart-admin": oidc.RoleAdmin}, "", nil, nil)
		cfg := baseTestConfig(newHarness(t), st)
		cfg.OIDC = auth
		return New(cfg), st
	}

	t.Run("an upsert reports the tokens it does not reach", func(t *testing.T) {
		srv, _ := newSrv(t)
		w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken,
			`{"value":"`+group+`","role":"member"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
		}
		var got struct {
			types.RoleMapping
			StaleTokenSnapshots int `json:"stale_token_snapshots"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.StaleTokenSnapshots != 1 {
			t.Errorf("stale_token_snapshots = %d, want 1 (alice's live group-snapshot token; carol's is revoked "+
				"and dave's names another group) — a demotion the admin cannot see is not yet effective is the "+
				"whole defect", got.StaleTokenSnapshots)
		}
		// STRICT SUPERSET: every field an existing client decodes is still
		// there, which is what makes this additive rather than a wire break.
		if got.Value != group || got.Role != string(oidc.RoleMember) || got.ID == uuid.Nil {
			t.Errorf("the response is no longer a RoleMapping superset: %+v", got)
		}
	})

	t.Run("the audit row carries the count", func(t *testing.T) {
		srv, _ := newSrv(t)
		if w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken,
			`{"value":"`+group+`","role":"member"}`); w.Code != http.StatusCreated {
			t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
		}
		h, ok := srv.cfg.Audit.(*recRecorder)
		if !ok {
			t.Fatal("audit recorder is not the test recorder")
		}
		ev := lastAuditEvent(t, h.events, "access.role_mapping.write")
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data["stale_token_snapshots"] != float64(1) {
			t.Errorf("audit data = %v, want stale_token_snapshots 1 — the audit trail is the system of record for "+
				"a demotion, and it could not say the demotion was incomplete", data)
		}
	})

	// The DELETE side is the sharper one: removing a mapping is how an admin
	// takes a role away. 204 carries no body, so the count rides the audit row.
	t.Run("a delete reports the count in its audit row", func(t *testing.T) {
		srv, st := newSrv(t)
		id := uuid.New()
		st.rows = []types.RoleMapping{{ID: id, Value: group, Role: oidc.RoleAdmin}}
		w := do(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+id.String()+"?acknowledge_access_change=true", adminToken, "")
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
		}
		h := srv.cfg.Audit.(*recRecorder)
		ev := lastAuditEvent(t, h.events, "access.role_mapping.delete")
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data["stale_token_snapshots"] != float64(1) {
			t.Errorf("audit data = %v, want stale_token_snapshots 1", data)
		}
	})

	// The control: a value nothing is bound to reports nothing, so the signal
	// means something when it does appear.
	t.Run("a value no token snapshot names reports zero", func(t *testing.T) {
		srv, _ := newSrv(t)
		w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken,
			`{"value":"nobody-team","role":"member"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
		}
		if got := w.Body.String(); jsonHasKey(t, got, "stale_token_snapshots") {
			t.Errorf("an unbound value reported the field anyway (%s) — omitempty keeps the wire quiet for the "+
				"deployments where there is nothing to say", got)
		}
	})
}

func jsonHasKey(t *testing.T, body, key string) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	_, ok := m[key]
	return ok
}
