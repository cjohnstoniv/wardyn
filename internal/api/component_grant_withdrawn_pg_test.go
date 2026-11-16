// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestLiveMintRefusedWhenAComponentGrantIsWithdrawn: a running run whose owner holds an organisation
// component through a group mints while the grant stands and the record of the owner's last sign-in
// names the group. Once the grant is withdrawn, a mint (and an injection redeem, the same door) is
// refused capability_component with a credential.mint denied row. With the grant back but no record,
// the group is not seen until a sign-in records it again. A SCIM removal from the group cuts the run the
// same way, and a SCIM add does not undo it: only a sign-in does. A deleted component refuses
// component_gone.
func TestLiveMintRefusedWhenAComponentGrantIsWithdrawn(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t), func(c *Config) {
		c.OIDC = newAccessAuth(t, map[string]string{"wardyn-admins": oidc.RoleAdmin}, oidc.RoleUser, nil, nil)
	})
	ctx := t.Context()
	const owner = "sub-gwm"
	c := seedOrgComponent(t, e, "gwm-tool")
	groupAllow := func() types.CapabilityGrant {
		t.Helper()
		g, err := e.pg.UpsertCapabilityGrant(ctx, grant(types.CapabilitySubjectGroup, "eng", capComponent, c.ID.String(), types.CapabilityAllow))
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	signIn := func() {
		t.Helper()
		if err := e.pg.PutPrincipalGroups(ctx, owner, "", "gwm@corp.example", []string{"eng"}, false, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	allow := groupAllow()
	signIn()
	run, err := e.pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: owner, Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunRunning, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.pg.PutRunComponents(ctx, run.ID, []types.RunComponent{{ComponentID: &c.ID, Name: c.Name, Version: c.Version, Definition: c.Definition}}); err != nil {
		t.Fatal(err)
	}
	g, err := e.pg.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID,
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(pgOrgScope)}})
	if err != nil {
		t.Fatal(err)
	}
	claims := &identity.Claims{RunID: run.ID, SPIFFEID: "spiffe://wardyn.test/agent-run/" + run.ID.String()}
	door := func() (*httptest.ResponseRecorder, bool) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/internal/v1/mint", nil)
		_, done := e.srv.refuseWithheldGrant(w, r, claims, g.ID, func(_ liveGrant, err error) { t.Fatalf("unread: %v", err) })
		return w, done
	}
	kept := func(step string) {
		t.Helper()
		if w, done := door(); done {
			t.Fatalf("%s: refused %d %s, want the credential given", step, w.Code, w.Body)
		}
	}
	refused := func(step, reason string) {
		t.Helper()
		w, done := door()
		if !done || w.Code != http.StatusForbidden || wireReason(t, w) != reason ||
			!strings.Contains(w.Body.String(), "wardyn: this run is no longer given credentials") {
			t.Fatalf("%s: %v %d %s, want 403 %s", step, done, w.Code, w.Body, reason)
		}
		rows := e.audits("credential.mint")
		if len(rows) == 0 || rows[len(rows)-1].Outcome != "denied" || !strings.Contains(string(rows[len(rows)-1].Data), reason) {
			t.Fatalf("%s: credential.mint rows = %+v, want a denied row with %s", step, rows, reason)
		}
	}

	kept("held through the owner's group")
	if _, err := e.pg.DeleteCapabilityGrant(ctx, allow.ID); err != nil {
		t.Fatal(err)
	}
	refused("grant withdrawn", reasonOwnerCapabilityComponent)

	groupAllow()
	if _, err := e.pg.DeleteLoginGroups(ctx, []string{owner}); err != nil {
		t.Fatal(err)
	}
	refused("no record of the owner's groups", reasonOwnerCapabilityComponent)
	signIn()
	kept("the owner signed in again")

	var ident uuid.UUID
	if err := e.pool.QueryRow(ctx, `INSERT INTO principal_identities (principal, issuer, email_lower) VALUES ($1, 'https://idp.test', 'gwm@corp.example') RETURNING id`,
		owner).Scan(&ident); err != nil {
		t.Fatal(err)
	}
	eng, err := e.pg.CreateScimGroup(ctx, "eng", "Engineering", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.pg.AddScimGroupMembers(ctx, eng.ID, []uuid.UUID{ident}, time.Now()); err != nil {
		t.Fatal(err)
	}
	kept("a member of the group by SCIM too")
	if err := e.pg.StartGroupRemoval(ctx, eng.ID, ident); err != nil {
		t.Fatal(err)
	}
	refused("removed from the group by SCIM", reasonOwnerCapabilityComponent)
	if err := e.pg.AddScimGroupMembers(ctx, eng.ID, []uuid.UUID{ident}, time.Now()); err != nil {
		t.Fatal(err)
	}
	refused("added back by SCIM, no sign-in since", reasonOwnerCapabilityComponent)
	signIn()
	kept("signed in again after the SCIM changes")

	if _, err := e.pg.DeleteComponent(ctx, c.ID, ""); err != nil {
		t.Fatal(err)
	}
	refused("component deleted", reasonOwnerComponentGone)
}
