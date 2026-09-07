// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// roleMapTokenStore is roleMapStore plus the api_tokens listing the stale-
// snapshot count reads.
type roleMapTokenStore struct {
	roleMapStore
	toks []types.APIToken
}

func (s *roleMapTokenStore) ListAPITokens(context.Context) ([]types.APIToken, error) {
	return slices.Clone(s.toks), nil
}

// ListAPITokensByPrincipal / RevokeAPIToken complete the double for the WRITE
// the demotion path now makes (revokeAPITokensFor): the sweep matches a
// principal by either identity, exactly as the PG store does, and a revoke
// stamps the row rather than deleting it.
func (s *roleMapTokenStore) ListAPITokensByPrincipal(_ context.Context, principal string) ([]types.APIToken, error) {
	var out []types.APIToken
	for _, t := range s.toks {
		if t.Principal == principal || (t.Email != "" && strings.EqualFold(t.Email, principal)) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *roleMapTokenStore) RevokeAPIToken(_ context.Context, id uuid.UUID, principal string, at time.Time) (types.APIToken, error) {
	for i := range s.toks {
		if s.toks[i].ID != id || (principal != "" && s.toks[i].Principal != principal) {
			continue
		}
		if s.toks[i].RevokedAt != nil {
			return types.APIToken{}, store.ErrNotFound
		}
		when := at
		s.toks[i].RevokedAt = &when
		return s.toks[i], nil
	}
	return types.APIToken{}, store.ErrNotFound
}

// liveRoles reports the stamped role of every UNREVOKED token, keyed by
// principal — what a demotion is supposed to change.
func (s *roleMapTokenStore) liveRoles() map[string]string {
	out := map[string]string{}
	for _, t := range s.toks {
		if t.RevokedAt == nil {
			out[t.Principal] = t.Role
		}
	}
	return out
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
// This test covers the INFORMATIONAL half — the count in the two places an
// admin looks, the response and the audit row, plus a WARN for the operator who
// is not looking. It once described itself as the whole remedy ("NOT an
// auto-revoke"), and that was the finding's `expected` left undone: the owner
// adjudication is that a demotion must be EFFECTIVE, so the acting half lives
// in TestRoleMappingDemotionRevokesTheStampedTokens below. The self-DoS that
// argument feared is answered by scoping the revoke to the snapshots the edit
// actually demotes, not by declining to act.
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

// TestRoleMappingDemotionRevokesTheStampedTokens is F112's owner adjudication:
// "the role-mapping write and delete paths call revokeAPITokensFor for a
// principal whose derived role drops, so demotion is effective immediately".
//
// The finding's `expected` is a BOUND — a TTL, an expiry, or an automatic
// revoke on demotion — so that removing someone's admin actually removes it
// within a bounded time. Reporting a count was the "at minimum" clause, and it
// left the credential live: an outstanding wdn_ token's role is stamped at mint
// and read verbatim on every request, so an admin who deletes the mapping that
// granted admin has, until now, removed nothing.
//
// The controls are half the point. A revoke on a mapping edit could be a
// self-DoS with the blast radius of a group, so this pins what must NOT be
// revoked as hard as what must: a member-stamped CI credential naming the same
// group, an admin-stamped token naming a different group, and every token when
// the edit is a promotion.
func TestRoleMappingDemotionRevokesTheStampedTokens(t *testing.T) {
	const (
		demoted = "eng-team"
		other   = "other-team"
	)
	answerable := false
	now := time.Now().UTC()
	fixture := func() []types.APIToken {
		return []types.APIToken{
			// The credential the demotion exists to kill.
			{ID: uuid.New(), Principal: "sub-alice", Email: "alice@corp.example", Role: string(oidc.RoleAdmin),
				Groups: []string{demoted}, GroupsTruncated: &answerable, CreatedAt: now},
			// Same group, MEMBER stamp: nothing was taken from it, so killing it
			// would be the self-DoS the count's old doc feared.
			{ID: uuid.New(), Principal: "sub-bob", Email: "bob@corp.example", Role: string(oidc.RoleMember),
				Groups: []string{demoted}, GroupsTruncated: &answerable, CreatedAt: now},
			// Admin stamp, DIFFERENT group: this edit does not move their
			// derivation, so it is not this edit's business.
			{ID: uuid.New(), Principal: "sub-carol", Email: "carol@corp.example", Role: string(oidc.RoleAdmin),
				Groups: []string{other}, GroupsTruncated: &answerable, CreatedAt: now},
			// PF-26: an admin stamp whose group snapshot is nil/unanswerable —
			// invisible to the old slices.Contains(t.Groups, value) count, and
			// impossible to re-derive, so it fails CLOSED.
			{ID: uuid.New(), Principal: "sub-dan", Email: "dan@corp.example", Role: string(oidc.RoleAdmin),
				CreatedAt: now},
		}
	}

	newSrv := func(t *testing.T, rows []types.RoleMapping) (*Server, *roleMapTokenStore) {
		t.Helper()
		st := &roleMapTokenStore{toks: fixture()}
		st.rows = rows
		auth := newAccessAuth(t, map[string]string{"chart-admin": oidc.RoleAdmin}, oidc.RoleMember, nil, &st.roleMapStore)
		cfg := baseTestConfig(newHarness(t), st)
		cfg.OIDC = auth
		return New(cfg), st
	}
	wantRoles := map[string]string{
		"sub-bob":   string(oidc.RoleMember),
		"sub-carol": string(oidc.RoleAdmin),
	}
	assertOutcome := func(t *testing.T, st *roleMapTokenStore, revokedWant []string) {
		t.Helper()
		live := st.liveRoles()
		for _, p := range revokedWant {
			if _, still := live[p]; still {
				t.Errorf("%s still holds a live wdn_ token after the demotion — its role stamp is frozen at mint and "+
					"read verbatim on every request, so the admin removed nothing", p)
			}
		}
		for p, role := range wantRoles {
			if slices.Contains(revokedWant, p) {
				continue
			}
			if got, still := live[p]; !still || got != role {
				t.Errorf("%s's token was revoked by someone else's demotion (live=%v) — one People-screen edit must not "+
					"kill credentials it did not demote", p, live)
			}
		}
	}

	t.Run("deleting the mapping that granted admin revokes the stamped tokens", func(t *testing.T) {
		id := uuid.New()
		srv, st := newSrv(t, []types.RoleMapping{{ID: id, Value: demoted, Role: oidc.RoleAdmin}})
		w := do(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+id.String()+"?acknowledge_access_change=true", adminToken, "")
		if w.Code != http.StatusNoContent {
			t.Fatalf("delete = %d, want 204; body=%s", w.Code, w.Body.String())
		}
		assertOutcome(t, st, []string{"sub-alice", "sub-dan"})

		ev := lastAuditEvent(t, srv.cfg.Audit.(*recRecorder).events, "access.role_mapping.delete")
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data["tokens_revoked"] != float64(2) {
			t.Errorf("audit data = %v, want tokens_revoked 2 — the audit trail is the system of record for a demotion", data)
		}
	})

	t.Run("an upsert that lowers the role revokes them too", func(t *testing.T) {
		id := uuid.New()
		srv, st := newSrv(t, []types.RoleMapping{{ID: id, Value: demoted, Role: oidc.RoleAdmin}})
		w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken,
			`{"value":"`+demoted+`","role":"member","acknowledge_access_change":true}`)
		if w.Code != http.StatusOK && w.Code != http.StatusCreated {
			t.Fatalf("upsert = %d; body=%s", w.Code, w.Body.String())
		}
		assertOutcome(t, st, []string{"sub-alice", "sub-dan"})

		var got struct {
			TokensRevoked int `json:"tokens_revoked"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.TokensRevoked != 2 {
			t.Errorf("tokens_revoked = %d, want 2 — the admin who performs the demotion is told what it cost", got.TokensRevoked)
		}
	})

	// admin ⇒ security_admin is a DROP even though the tiers do not nest: an
	// admin stamp carries run reach a security_admin's does not.
	t.Run("admin to security_admin is a demotion", func(t *testing.T) {
		id := uuid.New()
		srv, st := newSrv(t, []types.RoleMapping{{ID: id, Value: demoted, Role: oidc.RoleAdmin}})
		if w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken,
			`{"value":"`+demoted+`","role":"security_admin","acknowledge_access_change":true}`); w.Code != http.StatusOK && w.Code != http.StatusCreated {
			t.Fatalf("upsert = %d; body=%s", w.Code, w.Body.String())
		}
		assertOutcome(t, st, []string{"sub-alice", "sub-dan"})
	})

	// The control that keeps the revoke honest: a PROMOTION takes nothing away,
	// so it must not touch a single credential.
	t.Run("a promotion revokes nothing", func(t *testing.T) {
		id := uuid.New()
		srv, st := newSrv(t, []types.RoleMapping{{ID: id, Value: demoted, Role: oidc.RoleMember}})
		if w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken,
			`{"value":"`+demoted+`","role":"admin","acknowledge_access_change":true}`); w.Code != http.StatusOK && w.Code != http.StatusCreated {
			t.Fatalf("upsert = %d; body=%s", w.Code, w.Body.String())
		}
		if live := st.liveRoles(); len(live) != 4 {
			t.Errorf("a promotion revoked credentials (live=%v, want all 4) — the revoke fires on demotion only", live)
		}
	})

	// And an edit of an unrelated value moves nobody's derivation.
	t.Run("an unrelated mapping edit revokes nothing", func(t *testing.T) {
		srv, st := newSrv(t, []types.RoleMapping{{ID: uuid.New(), Value: demoted, Role: oidc.RoleAdmin}})
		if w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken,
			`{"value":"sales-team","role":"member","acknowledge_access_change":true}`); w.Code != http.StatusCreated {
			t.Fatalf("upsert = %d; body=%s", w.Code, w.Body.String())
		}
		if live := st.liveRoles(); len(live) != 4 {
			t.Errorf("an unrelated edit revoked credentials (live=%v, want all 4)", live)
		}
	})
}
