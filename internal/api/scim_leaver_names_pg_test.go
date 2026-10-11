// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
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
// neither is reached. The suspension and the purge refuse, answer 5xx and say why in the audit trail.
func TestPG_SCIMLeaver_AnUnboundLeaversAmbiguousAddressReachesNobody(t *testing.T) {
	e := newSCIMEnv(t)
	var claimants []bystander
	for _, sub := range []string{"sub-claimant-1", "sub-claimant-2"} {
		e.seedSignIn(sub, earlyName)
		claimants = append(claimants, e.seedBystander(sub, earlyName))
	}
	id := e.postUserID(e.a, leaverOID, earlyName, earlyName)

	e.removeLeaver(t, id, http.StatusInternalServerError, http.StatusInternalServerError, claimants...)
	if e.identity(id).DeactivatedAt != nil {
		t.Error("the refused suspension deactivated the identity")
	}
	// DELETE completes the suspension first, so its refusal is the suspension's too.
	for req, n := range map[string]*scimNode{"suspend": e.a, "DELETE": e.b} {
		rows := e.rows(n, "person.deprovision")
		if len(rows) != 1 || rows[0].Outcome != "denied" || dataOf(t, rows[0])["reason"] != reasonOwnerAmbiguous || dataOf(t, rows[0])["kind"] != store.JobKindSuspend {
			t.Errorf("%s: person.deprovision rows = %+v, want one denied %s", req, rows, reasonOwnerAmbiguous)
		}
	}
}
