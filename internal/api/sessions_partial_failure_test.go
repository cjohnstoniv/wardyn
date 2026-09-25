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

// failingTokenStore revokes failAfter tokens, then fails.
type failingTokenStore struct {
	sessionTokenStore
	failAfter int
}

func (s *failingTokenStore) RevokeAPIToken(ctx context.Context, id uuid.UUID, by string, now time.Time) (types.APIToken, error) {
	if len(s.revoked) >= s.failAfter {
		return types.APIToken{}, errors.New("token store unavailable")
	}
	return s.sessionTokenStore.RevokeAPIToken(ctx, id, by, now)
}

// erroringSessionRevocations fails the cutoff write itself.
type erroringSessionRevocations struct{ fakeSessionRevocations }

func (*erroringSessionRevocations) RevokeSub(context.Context, string) error {
	return errors.New("revocations store unavailable")
}
func (*erroringSessionRevocations) RevokeAll(context.Context) error {
	return errors.New("revocations store unavailable")
}

// TestRevokeSessions_PartialFailureIsAuditedAsFailure: the sessions are cut and
// some tokens with them before the token sweep fails. That half-applied
// security action must reach the append-only log as a failure row naming how
// far it got, and the caller must see a 500, not the 204 of a complete revoke.
func TestRevokeSessions_PartialFailureIsAuditedAsFailure(t *testing.T) {
	for _, c := range []struct {
		name, body, target, scope string
	}{
		{"sub", `{"sub":"sub-alice"}`, "sub-alice", "sub"},
		{"all", `{"all":true}`, "*", "all"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			st := &failingTokenStore{failAfter: 2, sessionTokenStore: sessionTokenStore{toks: []types.APIToken{
				{ID: uuid.New(), Principal: "sub-alice"},
				{ID: uuid.New(), Principal: "sub-alice"},
				{ID: uuid.New(), Principal: "sub-alice"},
			}}}
			cfg := baseTestConfig(h, st)
			cfg.OIDC = &oidc.Authenticator{}
			cfg.SessionRevocations = &fakeSessionRevocations{}
			srv := New(cfg)
			admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

			w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, c.body)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 for a half-applied revoke; body=%s", w.Code, w.Body.String())
			}
			var rows []types.AuditEvent
			for _, ev := range h.audit.snapshot() {
				if ev.Action == "session.revoke" {
					rows = append(rows, ev)
				}
			}
			if len(rows) != 1 || rows[0].Outcome != "failure" || rows[0].Target != c.target {
				t.Fatalf("session.revoke rows = %+v, want one failure row on %q", rows, c.target)
			}
			var data map[string]any
			if err := json.Unmarshal(rows[0].Data, &data); err != nil {
				t.Fatalf("decode data: %v", err)
			}
			if data["tokens_revoked"] != float64(2) || data["scope"] != c.scope || data["error"] == nil || data["error"] == "" {
				t.Fatalf("failure row data = %v, want tokens_revoked=2, scope=%s and the error", data, c.scope)
			}
		})
	}
}

// TestRevokeSessions_CutoffWriteFailureIs500: if the cutoff itself cannot be
// written nothing was revoked, and the caller must not be told otherwise.
func TestRevokeSessions_CutoffWriteFailureIs500(t *testing.T) {
	for _, body := range []string{`{"sub":"sub-alice"}`, `{"all":true}`} {
		h := newHarness(t)
		st := &sessionTokenStore{toks: []types.APIToken{{ID: uuid.New(), Principal: "sub-alice"}}}
		cfg := baseTestConfig(h, st)
		cfg.OIDC = &oidc.Authenticator{}
		cfg.SessionRevocations = &erroringSessionRevocations{}
		srv := New(cfg)
		admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

		if w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, body); w.Code != http.StatusInternalServerError {
			t.Fatalf("%s: status = %d, want 500", body, w.Code)
		}
		if len(st.revoked) != 0 {
			t.Errorf("%s: %d tokens revoked after the cutoff write failed", body, len(st.revoked))
		}
		for _, ev := range h.audit.snapshot() {
			if ev.Action == "session.revoke" && ev.Outcome == "success" {
				t.Errorf("%s: a success row for a revoke that never happened: %+v", body, ev)
			}
		}
	}
}
