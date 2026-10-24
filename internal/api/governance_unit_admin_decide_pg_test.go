// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newUnitAdminDecideEnv is the unit-admin tree on a store that runs *hook once, between the approver's
// pre-check and the decision transaction (beforeDecideStore). The hook is cleared before it runs, so a
// write it makes through the API cannot fire it again.
func newUnitAdminDecideEnv(t *testing.T, hook *func()) *unitAdminEnv {
	t.Helper()
	return newUnitAdminEnv(t, false, func(c *Config) {
		c.Store = beforeDecideStore{PG: store.NewPG(c.Store.(store.PG).Pool), hook: func(context.Context) {
			if f := *hook; f != nil {
				*hook = nil
				f()
			}
		}}
	})
}

func (e *unitAdminEnv) stateOf(id uuid.UUID) string {
	e.t.Helper()
	ch, err := e.pg.GetGovernanceChange(context.Background(), id)
	if err != nil {
		e.t.Fatalf("read change %s: %v", id, err)
	}
	return ch.State
}

// TestUnitAdminApproverScopeRereadInTx: a unit admin's scope over a held change is read again inside
// the decision's transaction, for a session and for an API token. A grant revoked, or the change's unit
// re-parented out of the approver's subtree, after the pre-check passed refuses the approval and
// leaves the change pending; an approver still in scope applies it.
func TestUnitAdminApproverScopeRereadInTx(t *testing.T) {
	var hook func()
	e := newUnitAdminDecideEnv(t, &hook)
	approver := func() (string, types.GovernanceUnitAdmin) {
		sub := e.fresh("sub-ua-approver")
		return sub, e.grant(e.a, types.CapabilitySubjectUser, sub)
	}
	revoke := func(g types.GovernanceUnitAdmin) func() {
		return func() {
			if _, err := store.RevokeUnitAdminQ(context.Background(), e.pool, g.UnitID, g.ID); err != nil {
				t.Errorf("revoke grant %s: %v", g.ID, err)
			}
		}
	}
	refusedPending := func(t *testing.T, what string, ch types.GovernanceChange, w *httptest.ResponseRecorder) {
		t.Helper()
		if !scopeRefused(t, w) {
			t.Fatalf("%s: approve = %d %s, want 403 unit_scope_denied", what, w.Code, w.Body)
		}
		if got := e.stateOf(ch.ID); got != types.GovernanceChangePending {
			t.Fatalf("%s: change state = %q, want pending", what, got)
		}
	}

	t.Run("session grant revoked", func(t *testing.T) {
		sub, g := approver()
		ch := e.heldOn(e.a2)
		hook = revoke(g)
		refusedPending(t, "session grant revoked", ch, e.call(uaSession(t, sub, []string{}, false), http.MethodPost, approvePath(ch.ID), ""))
	})

	t.Run("session unit re-parented out", func(t *testing.T) {
		// A held grant: its base is the grant row alone, so the re-parent does not make it stale (a
		// membership change's base holds the unit chain and would be), and only the scope read refuses.
		// Not a unit change either: its approval holds the site-config lock across the hook, and the
		// hook's PUT /units/{id} takes that same lock, so the test would deadlock on itself.
		sub, _ := approver()
		proposer, _ := approver()
		target := e.child(e.a)
		ch := e.pending(e.call(uaSession(t, proposer, []string{}, false), http.MethodPost,
			"/api/v1/governance/units/"+target.ID.String()+"/admins", `{"subject_type":"user","subject":"`+e.fresh("sub-ua-grantee")+`"}`))
		hook = func() {
			if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+target.ID.String(), e.restate(target, map[string]any{"parent_id": e.b.ID})); w.Code != http.StatusOK {
				t.Errorf("re-parent %s under ua-b = %d %s, want 200", target.Name, w.Code, w.Body)
			}
		}
		refusedPending(t, "session unit re-parented out", ch, e.call(uaSession(t, sub, []string{}, false), http.MethodPost, approvePath(ch.ID), ""))
	})

	t.Run("token grant revoked", func(t *testing.T) {
		sub, g := approver()
		_, raw := e.mintToken(sub, sub+"@corp.example", oidc.RoleUser)
		ch := e.heldOn(e.a2)
		hook = revoke(g)
		refusedPending(t, "token grant revoked", ch, do(t, e.srv, http.MethodPost, approvePath(ch.ID), raw, ""))
	})

	// A cli_access token publishes the role user whatever role its row holds, at the door and in the
	// re-check alike: its stored security-admin role is no approval after its grant is revoked.
	cliAccess := func(sub string) string {
		raw := apiTokenPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
		notTruncated := false
		if _, err := e.pg.CreateAPIToken(context.Background(), types.APIToken{
			ID: uuid.New(), Principal: sub, Email: sub + "@corp.example", Role: oidc.RoleSecurityAdmin, UserType: types.UserTypeStandard,
			Name: "approver", GroupsTruncated: &notTruncated, Kind: types.APITokenKindCLIAccess,
		}, raw); err != nil {
			t.Fatalf("mint cli_access token: %v", err)
		}
		return raw
	}
	added := func(ch types.GovernanceChange) bool {
		unit, _, subject, _ := splitMembershipKey(ch.TargetKey)
		ms, err := e.pg.ListGovernanceMemberships(context.Background())
		if err != nil {
			t.Fatalf("list memberships: %v", err)
		}
		return slices.ContainsFunc(ms, func(m types.GovernanceMembership) bool { return m.UnitID == unit && m.Subject == subject })
	}

	t.Run("cli_access token with an elevated role, grant revoked", func(t *testing.T) {
		sub, g := approver()
		raw := cliAccess(sub)
		ch := e.heldOn(e.a2)
		hook = revoke(g)
		refusedPending(t, "cli_access grant revoked", ch, do(t, e.srv, http.MethodPost, approvePath(ch.ID), raw, ""))
		if added(ch) {
			t.Fatalf("cli_access grant revoked: the held membership was written")
		}
	})

	t.Run("cli_access token with an elevated role, in scope", func(t *testing.T) {
		sub, _ := approver()
		ch := e.heldOn(e.a2)
		if w := do(t, e.srv, http.MethodPost, approvePath(ch.ID), cliAccess(sub), ""); w.Code != http.StatusOK || !added(ch) {
			t.Fatalf("cli_access approve in scope = %d %s, want 200 and the membership written", w.Code, w.Body)
		}
	})

	t.Run("in scope applies", func(t *testing.T) {
		sub, _ := approver()
		_, raw := e.mintToken(sub, sub+"@corp.example", oidc.RoleUser)
		byToken := e.heldOn(e.a2)
		if w := do(t, e.srv, http.MethodPost, approvePath(byToken.ID), raw, ""); w.Code != http.StatusOK {
			t.Fatalf("token approve in scope = %d %s, want 200", w.Code, w.Body)
		}
		bySession := e.heldOn(e.a2)
		if w := e.call(uaSession(t, sub, []string{}, false), http.MethodPost, approvePath(bySession.ID), ""); w.Code != http.StatusOK {
			t.Fatalf("session approve in scope = %d %s, want 200", w.Code, w.Body)
		}
		for _, id := range []uuid.UUID{byToken.ID, bySession.ID} {
			if got := e.stateOf(id); got == types.GovernanceChangePending {
				t.Fatalf("change %s still pending after an in-scope approval", id)
			}
		}
	})
}

// scopeReadStore runs *afterRead once, on the decision's own goroutine, as soon as the decision
// transaction's first unit-scope read (store.UnitScopeQ's statement) has returned: the window between
// the approver's scope re-read and the decision's commit.
type scopeReadStore struct {
	store.PG
	afterRead *func()
}

func (s scopeReadStore) DecideGovernanceChange(ctx context.Context, id uuid.UUID, d store.GovernanceDecision, fn store.GovernanceDecideFunc) (types.GovernanceChange, error) {
	return s.PG.DecideGovernanceChange(ctx, id, d, func(q store.Querier, ch types.GovernanceChange) error {
		return fn(scopeReadQuerier{Querier: q, afterRead: s.afterRead}, ch)
	})
}

type scopeReadQuerier struct {
	store.Querier
	afterRead *func()
}

func (q scopeReadQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := q.Querier.QueryRow(ctx, sql, args...)
	if !strings.Contains(sql, "WITH RECURSIVE chain") || !strings.Contains(sql, "governance_unit_admins") {
		return row
	}
	return scopeReadRow{Row: row, afterRead: q.afterRead}
}

type scopeReadRow struct {
	pgx.Row
	afterRead *func()
}

func (r scopeReadRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	if f := *r.afterRead; f != nil {
		*r.afterRead = nil
		f()
	}
	return err
}

// TestUnitAdminDecisionSerializesScopeWrites: a decision's scope re-read and its commit are one
// section under the graph lock. A direct revoke of the approver's own grant, sent once the re-read has
// returned, waits for the decision to commit instead of landing between the two.
func TestUnitAdminDecisionSerializesScopeWrites(t *testing.T) {
	var afterRead func()
	e := newUnitAdminEnv(t, false, func(c *Config) {
		c.Store = scopeReadStore{PG: store.NewPG(c.Store.(store.PG).Pool), afterRead: &afterRead}
	})
	sub := e.fresh("sub-ua-approver")
	g := e.grant(e.a, types.CapabilitySubjectUser, sub)
	ch := e.heldOn(e.a2)
	revoked := make(chan int, 1)
	landed := false
	afterRead = func() {
		go func() {
			revoked <- do(t, e.srv, http.MethodDelete, "/api/v1/governance/units/"+e.a.ID.String()+"/admins/"+g.ID.String(), adminToken, "").Code
		}()
		select {
		case code := <-revoked:
			landed = true
			revoked <- code
		case <-time.After(500 * time.Millisecond):
		}
	}
	w := e.call(uaSession(t, sub, []string{}, false), http.MethodPost, approvePath(ch.ID), "")
	if landed {
		t.Fatalf("the approver's grant was revoked between the decision's scope read and its commit (approve = %d %s)", w.Code, w.Body)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s, want 200 with the revoke waiting for it", w.Code, w.Body)
	}
	select {
	case code := <-revoked:
		if code != http.StatusNoContent {
			t.Fatalf("the revoke after the decision = %d, want 204", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revoke did not finish after the decision committed")
	}
}
