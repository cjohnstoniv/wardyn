// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A SCIM leaver's address can spell another person: the principal of a person an admin created
// ("early@corp.example", not yet signed in), or an email another person claimed at sign-in and carries on
// an API token. Suspending and purging the leaver must reach only the person store.PrincipalsNamed says
// the leaver is, never that other person.

const (
	earlyName  = "early@corp.example"
	leaverOID  = "aaaaaaaa-0000-4000-8000-0000000000c1"
	claimerSub = "sub-claimant"
)

// bystander is everything one person holds that a suspension or a purge could reach.
type bystander struct {
	principal string
	tokenID   uuid.UUID
	tokenRaw  string
	key       types.SSHPublicKey
	run       uuid.UUID
	cookie    *http.Cookie
	workspace uuid.UUID
	// claimsAddress is a bystander whose sign-in claimed the leaver's address: a cutoff written under that
	// address (the principal it names is the leaver) also matches their session's email claim, by
	// IsSessionRevoked's email arm, so their sessions and tokens are not checked.
	claimsAddress bool
}

// seedBystander gives principal a token (carrying email), an SSH key, a running run, a session, a
// stored credential, a workspace, and a deny grant and an assignment keyed by the principal.
func (e *scimEnv) seedBystander(principal, email string) bystander {
	e.t.Helper()
	ctx := context.Background()
	b := bystander{principal: principal, run: e.seedRun(principal, types.RunRunning), cookie: scimCookie(e.t, principal, email, 0)}
	b.tokenID, b.tokenRaw = e.seedToken(principal, email)
	b.key = e.key(e.seedKey(principal))
	if err := e.sec.For(principal).Put(ctx, "api-key", []byte("s3cret")); err != nil {
		e.t.Fatal(err)
	}
	ws, err := e.st.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "ws-" + principal, OwnedBy: principal, Status: types.WorkspaceScanned,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	b.workspace = ws.ID
	if _, err := e.st.UpsertCapabilityGrant(ctx, types.CapabilityGrant{
		SubjectType: types.CapabilitySubjectUser, Subject: principal, Capability: "egress_domain", Value: "evil.example", Effect: types.CapabilityDeny,
	}); err != nil {
		e.t.Fatal(err)
	}
	prof, err := e.st.UpsertGovernanceProfile(ctx, types.GovernanceProfile{ID: uuid.New(), Name: "walled-" + principal, Ceiling: govProfileSpec()})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.UpsertGovernanceAssignment(ctx, types.GovernanceAssignment{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: principal, ProfileID: prof.ID,
	}); err != nil {
		e.t.Fatal(err)
	}
	return b
}

// untouched reports, after step, everything of b's that the leaver's removal reached.
func (e *scimEnv) untouched(t *testing.T, step string, b bystander, purged bool) {
	t.Helper()
	ctx := context.Background()
	for name, n := range map[string]*scimNode{"a": e.a, "b": e.b} {
		if !b.claimsAddress && !e.cookieWorks(n, b.cookie) {
			t.Errorf("%s, instance %s: %s's session was cut", step, name, b.principal)
		}
		if !b.claimsAddress && !e.tokenWorks(n, b.tokenRaw) {
			t.Errorf("%s, instance %s: %s's token stopped authenticating", step, name, b.principal)
		}
		if why := n.srv.sshKeyRevocationRefusal(ctx, b.key); why != "" {
			t.Errorf("%s, instance %s: %s's SSH key is refused: %s", step, name, b.principal, why)
		}
	}
	if !b.claimsAddress && e.tokenRevoked(b.tokenID) {
		t.Errorf("%s: %s's token was revoked", step, b.principal)
	}
	if e.keyCount(b.principal) != 1 {
		t.Errorf("%s: %s's SSH key was deleted", step, b.principal)
	}
	if st := e.runState(b.run); st != types.RunRunning {
		t.Errorf("%s: %s's run is %s, want RUNNING", step, b.principal, st)
	}
	if g, a := e.userRows(b.principal); g != 1 || a != 1 {
		t.Errorf("%s: %s has %d grants and %d assignments, want 1 and 1", step, b.principal, g, a)
	}
	if !purged {
		return
	}
	if e.credentialCount(b.principal) != 1 {
		t.Errorf("%s: %s's stored credential was erased", step, b.principal)
	}
	ws, err := e.st.GetWorkspace(ctx, b.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if ws.OwnedBy != b.principal {
		t.Errorf("%s: %s's workspace was handed to %q", step, b.principal, ws.OwnedBy)
	}
}

// removeLeaver suspends, then purges, the SCIM user id, checking each answer and b after each.
func (e *scimEnv) removeLeaver(t *testing.T, id string, suspendCode, deleteCode int, b ...bystander) {
	t.Helper()
	if w := e.patch(e.a, id, patchOf(`"False"`)); w.Code != suspendCode {
		t.Errorf("suspend = %d %s, want %d", w.Code, w.Body.String(), suspendCode)
	}
	for _, p := range b {
		e.untouched(t, "suspend", p, false)
	}
	if w := e.del(e.b, id); w.Code != deleteCode {
		t.Errorf("DELETE = %d %s, want %d", w.Code, w.Body.String(), deleteCode)
	}
	for _, p := range b {
		e.untouched(t, "purge", p, true)
	}
}

// A bound leaver whose identity provider sends an address that is another principal: a person an admin
// created under it, or a principal that only its own API token and workspace record.
func TestPG_SCIMLeaver_ABoundLeaversAddressNeverReachesThePersonItNames(t *testing.T) {
	for name, created := range map[string]bool{"a created person": true, "a principal with no person row": false} {
		t.Run(name, func(t *testing.T) {
			e := newSCIMEnv(t)
			if created {
				if _, _, err := e.st.CreatePerson(context.Background(), types.Person{Principal: earlyName, CreatedBy: "admin"}); err != nil {
					t.Fatal(err)
				}
			}
			early := e.seedBystander(earlyName, "")
			e.seedEntra("sub-leaver", earlyName, leaverOID)
			id := e.postUserID(e.a, leaverOID, earlyName, earlyName)
			_, leaverRaw := e.seedToken("sub-leaver", earlyName)

			e.removeLeaver(t, id, http.StatusOK, http.StatusNoContent, early)
			if e.tokenWorks(e.a, leaverRaw) {
				t.Error("the leaver's own token still authenticates")
			}
		})
	}
}

// An unbound leaver (no sign-in bound the SCIM user) whose address a created person holds as their
// principal and another person claimed at sign-in, carried on their API token: the address names the
// created person, so the claimant's runs, SSH key, credential, grants and workspace are never reached.
func TestPG_SCIMLeaver_AnUnboundLeaversAddressNeverReachesATokenEmailClaimant(t *testing.T) {
	e := newSCIMEnv(t)
	if _, _, err := e.st.CreatePerson(context.Background(), types.Person{Principal: earlyName, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	claimant := e.seedBystander(claimerSub, earlyName)
	claimant.claimsAddress = true
	id := e.postUserID(e.a, leaverOID, earlyName, earlyName)

	e.removeLeaver(t, id, http.StatusOK, http.StatusNoContent, claimant)
}

// An unbound leaver whose address two people claimed at sign-in: the address names nobody alone, so
// neither is reached. The suspension deactivates the leaver's identity row alone; it and the purge refuse
// the rest, answer 5xx and say why in the audit trail.
func TestPG_SCIMLeaver_AnUnboundLeaversAmbiguousAddressReachesNobody(t *testing.T) {
	e := newSCIMEnv(t)
	var claimants []bystander
	for _, sub := range []string{"sub-claimant-1", "sub-claimant-2"} {
		e.seedSignIn(sub, earlyName)
		claimants = append(claimants, e.seedBystander(sub, earlyName))
	}
	id := e.postUserID(e.a, leaverOID, earlyName, earlyName)

	e.removeLeaver(t, id, http.StatusInternalServerError, http.StatusInternalServerError, claimants...)
	if e.identity(id).DeactivatedAt == nil {
		t.Error("the refused suspension left the leaver's identity active")
	}
	// DELETE completes the suspension first, so its refusal is the suspension's: step 1 already stopped for this
	// reason on the suspend, and the retry writes no second row.
	rows := e.rows(e.a, "person.deprovision")
	if len(rows) != 1 || rows[0].Outcome != "denied" || dataOf(t, rows[0])["reason"] != reasonOwnerAmbiguous || dataOf(t, rows[0])["kind"] != store.JobKindSuspend {
		t.Errorf("suspend: person.deprovision rows = %+v, want one denied %s", rows, reasonOwnerAmbiguous)
	}
	if rows := e.rows(e.b, "person.deprovision"); len(rows) != 0 {
		t.Errorf("DELETE: person.deprovision rows = %+v, want none: the step already stopped for this reason", rows)
	}
}

// A SCIM user whose address is a created person's principal, when another person signed in claiming that
// address: the address names the created person, so the SCIM user never links to the claimant's sign-in
// row and stays unbound, and suspending and purging it never deprovisions the claimant.
func TestPG_SCIMLeaver_AnAddressThatIsAPrincipalNeverLinksAClaimantsSignIn(t *testing.T) {
	e := newSCIMEnv(t)
	if _, _, err := e.st.CreatePerson(context.Background(), types.Person{Principal: earlyName, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	row := e.seedSignIn(claimerSub, earlyName)
	claimant := e.seedBystander(claimerSub, earlyName)
	claimant.claimsAddress = true
	id := e.postUserID(e.a, leaverOID, earlyName, earlyName)
	if ident := e.identity(id); ident.ID == row.ID || ident.Principal != "" {
		t.Fatalf("the SCIM user linked to %q's sign-in row", ident.Principal)
	}

	e.removeLeaver(t, id, http.StatusOK, http.StatusNoContent, claimant)
	if got := e.identity(row.ID.String()); got.DeactivatedAt != nil || got.PurgedAt != nil {
		t.Error("the claimant's identity was deactivated or purged")
	}
}

// A SCIM user one of whose names is a created person's principal links to no sign-in row through any of
// its other names, in either order. Unbound, the user's emails then name two different principals (the
// created person and the claimant who signed in under the other address), so the suspension and the purge
// refuse: neither is touched, the request answers 5xx and a denied person.deprovision says why.
func TestPG_SCIMLeaver_AnyNameThatIsAPrincipalStopsTheLinkThroughEveryOtherName(t *testing.T) {
	const alias = "early.alias@corp.example"
	for name, names := range map[string][2]string{
		"the principal as userName": {earlyName, alias},
		"the principal as email":    {alias, earlyName},
	} {
		t.Run(name, func(t *testing.T) {
			e := newSCIMEnv(t)
			if _, _, err := e.st.CreatePerson(context.Background(), types.Person{Principal: earlyName, CreatedBy: "admin"}); err != nil {
				t.Fatal(err)
			}
			personRun := e.seedRun(earlyName, types.RunRunning)
			row := e.seedSignIn(claimerSub, alias)
			claimant := e.seedBystander(claimerSub, alias)
			id := e.postUserID(e.a, leaverOID, names[0], names[1])
			if ident := e.identity(id); ident.ID == row.ID || ident.Principal != "" {
				t.Fatalf("the SCIM user linked to %q's sign-in row", ident.Principal)
			}

			if w := e.patch(e.a, id, patchOf(`"False"`)); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "deprovisioning is incomplete") {
				t.Errorf("suspend = %d %s, want 500 deprovisioning is incomplete", w.Code, w.Body.String())
			}
			e.untouched(t, "suspend", claimant, false)
			if w := e.del(e.b, id); w.Code != http.StatusInternalServerError {
				t.Errorf("DELETE = %d %s, want 500", w.Code, w.Body.String())
			}
			e.untouched(t, "purge", claimant, true)
			if got := e.identity(row.ID.String()); got.DeactivatedAt != nil || got.PurgedAt != nil {
				t.Error("the claimant's identity was deactivated or purged in the leaver's place")
			}
			if st := e.runState(personRun); st != types.RunRunning {
				t.Errorf("the created person's run is %s, want RUNNING", st)
			}
			rows := e.rows(e.a, "person.deprovision")
			if len(rows) != 1 || rows[0].Outcome != "denied" || dataOf(t, rows[0])["reason"] != reasonOwnerAmbiguous {
				t.Errorf("person.deprovision rows = %+v, want one denied %s", rows, reasonOwnerAmbiguous)
			}
		})
	}
}
