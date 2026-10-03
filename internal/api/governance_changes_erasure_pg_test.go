// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_GovernanceChanges_ErasureClearsTheSubjectAndRetiresTheirPendingChanges: after
// POST /people/{p}/erasure for the proposer (audit_personal_fields), a direct store read of their
// pending and decided rows shows the four personal fields erased, and the formerly pending row is no
// longer pending, so a change with no recorded proposer can never be approved. A person who only
// decided a change is cleared too; the other party's own fields are left alone.
func TestPG_GovernanceChanges_ErasureClearsTheSubjectAndRetiresTheirPendingChanges(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	l := newMaskLab(t)
	fail := false
	a := erasureReplica(l, &fail)
	pg := store.NewPG(l.pool)
	ctx := context.Background()

	alice := ssoSession(t, "alice-sub", "alice@corp.example", oidc.RoleSecurityAdmin)
	bob := ssoSession(t, "bob-sub", "bob@corp.example", oidc.RoleSecurityAdmin)
	sec := ssoSession(t, "sec-sub", "sec@corp.example", oidc.RoleSecurityAdmin)

	seed := func(name string) types.GovernanceProfile {
		p, err := pg.UpsertGovernanceProfile(ctx, types.GovernanceProfile{
			Name: name, CreatedBy: "seed", Ceiling: types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}, MinConfinementClass: types.CC2},
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	propose := func(who *http.Cookie, p types.GovernanceProfile) uuid.UUID {
		t.Helper()
		w := doSSO(t, a.srv, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), who, profileBody(p.Name, "pypi.org", "github.com"))
		if w.Code != http.StatusAccepted {
			t.Fatalf("propose %s = %d %s, want 202", p.Name, w.Code, w.Body)
		}
		var id uuid.UUID
		if err := l.pool.QueryRow(ctx, `SELECT id FROM governance_changes WHERE target_key = $1 AND state = 'pending'`, p.ID.String()).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	decide := func(who *http.Cookie, id uuid.UUID) {
		t.Helper()
		if w := doSSO(t, a.srv, http.MethodPost, approvePath(id), who, ""); w.Code != http.StatusOK {
			t.Fatalf("approve = %d %s, want 200", w.Code, w.Body)
		}
	}
	pendingByAlice := propose(alice, seed("p-pending"))
	decidedProposedByAlice := propose(alice, seed("p-alice-proposed"))
	decide(bob, decidedProposedByAlice)
	decidedByAlice := propose(bob, seed("p-alice-decided"))
	decide(alice, decidedByAlice)
	bobsOwnPending := propose(bob, seed("p-bob-pending"))

	type row struct{ state, by, byEmail, decidedBy, decidedByEmail string }
	read := func(id uuid.UUID) row {
		t.Helper()
		var r row
		if err := l.pool.QueryRow(ctx, `SELECT state, proposed_by, proposed_by_email, decided_by, decided_by_email FROM governance_changes WHERE id = $1`, id).
			Scan(&r.state, &r.by, &r.byEmail, &r.decidedBy, &r.decidedByEmail); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := read(pendingByAlice); r.by != "alice-sub" || r.byEmail != "alice@corp.example" || r.state != types.GovernanceChangePending {
		t.Fatalf("before the erasure the pending row = %+v", r)
	}

	w := doSSO(t, a.srv, http.MethodPost, "/api/v1/people/alice-sub/erasure", sec, erasureBody("audit_personal_fields"))
	if w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s, want 200", w.Code, w.Body)
	}

	if r := read(pendingByAlice); r.by != "" || r.byEmail != "" || r.state == types.GovernanceChangePending {
		t.Errorf("alice's pending change = %+v, want her fields erased and the row no longer pending", r)
	}
	if r := read(decidedProposedByAlice); r.by != "" || r.byEmail != "" || r.decidedBy != "bob-sub" || r.state != types.GovernanceChangeApplied {
		t.Errorf("a change alice proposed = %+v, want her fields erased, bob's decision and the state kept", r)
	}
	if r := read(decidedByAlice); r.decidedBy != "" || r.decidedByEmail != "" || r.by != "bob-sub" || r.state != types.GovernanceChangeApplied {
		t.Errorf("a change alice decided = %+v, want her decision erased and bob's proposal kept", r)
	}
	if r := read(bobsOwnPending); r.by != "bob-sub" || r.byEmail != "bob@corp.example" || r.state != types.GovernanceChangePending {
		t.Errorf("bob's pending change = %+v, want it untouched", r)
	}

	// A change with no recorded proposer is never approvable.
	w = doSSO(t, a.srv, http.MethodPost, approvePath(pendingByAlice), bob, "")
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeNotPending {
		t.Errorf("approving alice's erased change = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeNotPending)
	}
	// The erasure is idempotent: a retry changes nothing and still succeeds.
	if w := doSSO(t, a.srv, http.MethodPost, "/api/v1/people/alice-sub/erasure", sec, erasureBody("audit_personal_fields")); w.Code != http.StatusOK {
		t.Errorf("a retried erasure = %d %s, want 200", w.Code, w.Body)
	}
}
