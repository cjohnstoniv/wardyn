// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// outageTokenStore is roleMapTokenStore whose token reads or revokes can be
// made to fail, the way a store outage would after the mapping edit is durable.
type outageTokenStore struct {
	roleMapTokenStore
	listErr, revokeErr error
}

func (s *outageTokenStore) ListAPITokens(ctx context.Context) ([]types.APIToken, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.roleMapTokenStore.ListAPITokens(ctx)
}

func (s *outageTokenStore) RevokeAPIToken(ctx context.Context, id uuid.UUID, principal string, at time.Time) (types.APIToken, error) {
	if s.revokeErr != nil {
		return types.APIToken{}, s.revokeErr
	}
	return s.roleMapTokenStore.RevokeAPIToken(ctx, id, principal, at)
}

// #622: a demotion whose outstanding tokens cannot be listed (or revoked) has
// already changed the mapping, so it is not undone, but it must not read as
// "0 tokens revoked": the audit row says the change has not reached the tokens,
// and so does the upsert's response (the delete's 204 has no body). A demotion
// that does reach them says nothing of it.
func TestRoleMappingDemotion_SaysWhenTheTokensWereNotReached(t *testing.T) {
	const demoted = "eng-team"
	build := func(t *testing.T, listErr, revokeErr error) (*Server, uuid.UUID) {
		t.Helper()
		id := uuid.New()
		st := &outageTokenStore{listErr: listErr, revokeErr: revokeErr}
		st.toks = []types.APIToken{{ID: uuid.New(), Principal: "sub-alice", Email: "alice@corp.example", Role: string(oidc.RoleAdmin), CreatedAt: time.Now().UTC()}}
		st.rows = []types.RoleMapping{{ID: id, Value: demoted, Role: oidc.RoleAdmin}}
		auth := newAccessAuth(t, map[string]string{"chart-admin": oidc.RoleAdmin}, oidc.RoleUser, nil, &st.roleMapStore)
		cfg := baseTestConfig(newHarness(t), st)
		cfg.OIDC = auth
		return New(cfg), id
	}
	auditData := func(t *testing.T, srv *Server, action string) map[string]any {
		t.Helper()
		var data map[string]any
		if err := json.Unmarshal(lastAuditEvent(t, srv.cfg.Audit.(*recRecorder).events, action).Data, &data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	body := func(t *testing.T, raw []byte) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("body %q: %v", raw, err)
		}
		return m
	}
	outage := errors.New("store unavailable")
	for name, tc := range map[string]struct{ listErr, revokeErr error }{
		"the tokens cannot be listed": {listErr: outage},
		"a token cannot be revoked":   {revokeErr: outage},
	} {
		t.Run(name+", on delete", func(t *testing.T) {
			srv, id := build(t, tc.listErr, tc.revokeErr)
			w := do(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+id.String()+"?acknowledge_access_change=true", adminToken, "")
			// The delete's 204 has no body and keeps none: the audit row carries it.
			if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
				t.Fatalf("delete = %d %q, want the unchanged 204", w.Code, w.Body.String())
			}
			if d := auditData(t, srv, "access.role_mapping.delete"); d["tokens_revocation_failed"] != true || d["tokens_revoked"] != float64(0) {
				t.Errorf("audit data = %v, want tokens_revocation_failed true and tokens_revoked 0", d)
			}
		})
		t.Run(name+", on upsert", func(t *testing.T) {
			srv, _ := build(t, tc.listErr, tc.revokeErr)
			w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken,
				`{"value":"`+demoted+`","role":"user","acknowledge_access_change":true}`)
			if w.Code != http.StatusOK || body(t, w.Body.Bytes())["tokens_revocation_failed"] != true {
				t.Fatalf("upsert = %d %q, want 200 saying tokens_revocation_failed", w.Code, w.Body.String())
			}
			if d := auditData(t, srv, "access.role_mapping.write"); d["tokens_revocation_failed"] != true {
				t.Errorf("audit data = %v, want tokens_revocation_failed true", d)
			}
		})
	}
	t.Run("nothing failed, nothing said", func(t *testing.T) {
		srv, id := build(t, nil, nil)
		w := do(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+id.String()+"?acknowledge_access_change=true", adminToken, "")
		if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
			t.Fatalf("delete = %d %q, want the unchanged 204", w.Code, w.Body.String())
		}
		d := auditData(t, srv, "access.role_mapping.delete")
		if _, said := d["tokens_revocation_failed"]; said || d["tokens_revoked"] != float64(1) {
			t.Errorf("audit data = %v, want tokens_revoked 1 and no tokens_revocation_failed", d)
		}
	})
}
