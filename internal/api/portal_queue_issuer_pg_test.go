// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// roleSpyStore counts the console role-mapping reads a start makes. It wraps the concrete store, so every
// optional interface the queue and the portal readers assert on is still there.
type roleSpyStore struct {
	store.PG
	roleReads atomic.Int64
}

func (s *roleSpyStore) ListRoleMappings(ctx context.Context) ([]types.RoleMapping, error) {
	s.roleReads.Add(1)
	return s.PG.ListRoleMappings(ctx)
}

// issuerQueueFixture is an issuer portal, its person, and a queued launch rewritten to carry the person's
// issuer-portal authority: sub the portal principal, the grant encoded portal:<uuid>.
type issuerQueueFixture struct {
	portal    types.Delegate
	principal string
}

func (e *queueEnv) issuerPortal(t *testing.T, pin *uuid.UUID, shape ...func(*types.Delegate)) issuerQueueFixture {
	t.Helper()
	ctx := context.Background()
	b, err := e.pg.BindPortalIssuerQ(ctx, e.pg.Pool, "https://idp-"+uuid.NewString()+".example.com", "https://internal-"+uuid.NewString()+".example.com", "uid", "test")
	if err != nil {
		t.Fatal(err)
	}
	d := types.Delegate{ID: uuid.New(), Name: "portal-" + uuid.NewString(), IdPClientID: uuid.NewString(), RegisteredBy: "test",
		Issuer: b.Issuer, InternalIssuer: b.InternalIssuerNorm, IssuerKey: b.IssuerKey, RunInUnder: pin,
		TokenProfile: "okta_access_token", UserType: types.UserTypeStandard, PortalManagedAdmission: true,
		MaxSubjectTokenAgeSeconds: 600, GrantTTLSeconds: 600}
	for _, f := range shape {
		f(&d)
	}
	d, err = e.pg.CreateDelegateQ(ctx, e.pg.Pool, d, sha256HexOf(store.PortalCredentialPrefix+uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	return issuerQueueFixture{portal: d, principal: types.PortalPrincipal(d.IssuerKey, "alice")}
}

// queued launches into pool as e.person and stores the portal person's authority on the queue row.
func (e *queueEnv) issuerQueued(t *testing.T, pool types.RunnerPool, f issuerQueueFixture) queuedLaunchAnswer {
	t.Helper()
	run := e.queuedLaunch(e.person, pool, "")
	e.exec(`UPDATE run_queue SET authority = authority || jsonb_build_object('sub', $2::text, 'role', 'user', 'user_type', $3::text,
		'delegate', $4::text, 'grant', 'portal:' || $5::text) WHERE run_id = $1`,
		run.ID, f.principal, f.portal.UserType, f.portal.ID.String(), uuid.NewString())
	// The run row froze its creator's type at launch; a portal person's launch would have frozen the portal's.
	e.exec(`UPDATE agent_runs SET user_type = $2 WHERE id = $1`, run.ID, f.portal.UserType)
	return run
}

// portalType creates a user type that no console role mapping gives anyone, for a portal to give its people.
func (e *queueEnv) portalType(t *testing.T) string {
	t.Helper()
	id := "portal-" + uuid.NewString()[:8]
	if _, err := e.pg.CreateUserType(context.Background(), types.UserType{ID: id, Name: id}); err != nil {
		t.Fatal(err)
	}
	return id
}

// A queued issuer-portal launch starts as role user under the portal row's own, distinct user type: the
// console's role mappings are never read, whatever they would derive for the empty email and groups, and the
// started run carries the portal's type.
func TestPG_QueuePortalIssuerLaunch_NoConsoleRoleRead(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	kind := e.portalType(t)
	f := e.issuerPortal(t, nil, func(d *types.Delegate) { d.UserType = kind })
	occupant := e.occupy(pool)
	run := e.issuerQueued(t, pool, f)
	spy := &roleSpyStore{PG: e.pg}
	e.srv.cfg.Store = spy
	e.free(occupant)
	started := e.waitStarted(run.ID)
	if n := spy.roleReads.Load(); n != 0 {
		t.Errorf("the start read the console role mappings %d times, want none", n)
	}
	if started.UserType != kind {
		t.Errorf("the started run's user type = %q, want the portal's %q", started.UserType, kind)
	}
	rows := e.audits(started.ID, "run.queue.start")
	if len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("run.queue.start rows = %+v, want one success", rows)
	}
	// The launch's own run.create row predates the type the fixture froze on the run, so a row that names it
	// is the start's.
	carried := false
	for _, ev := range e.audits(started.ID, "run.create") {
		carried = carried || strings.Contains(string(ev.Data), `"user_type":"`+kind+`"`)
	}
	if !carried {
		t.Errorf("no run.create row of the started run carries the portal's user type %q", kind)
	}
}

// The start judges the person by the portal's user type, not by anything the console would map them to: a type
// whose agent door is shut ends the run at its start.
func TestPG_QueuePortalIssuerLaunch_JudgedByPortalType(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	kind := e.portalType(t)
	if _, err := e.pg.UpsertCapabilityGrant(context.Background(), types.CapabilityGrant{SubjectType: types.CapabilitySubjectUserType,
		Subject: kind, Capability: capAgent, Value: "claude-code", Effect: types.CapabilityDeny, CreatedBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	f := e.issuerPortal(t, nil, func(d *types.Delegate) { d.UserType = kind })
	occupant := e.occupy(pool)
	run := e.issuerQueued(t, pool, f)
	e.free(occupant)
	if got := e.waitFailed(run.ID); got.StatusDetail != "queue: capability_agent" {
		t.Errorf("a portal person of a type with the agent door shut ended %q, want queue: capability_agent", got.StatusDetail)
	}
}

// The portal's user type was deleted while the run waited: the run ends user_type_unknown and never starts.
func TestPG_QueuePortalIssuerLaunch_PortalTypeDeletedEnds(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	f := e.issuerPortal(t, nil, func(d *types.Delegate) { d.UserType = "gone-" + uuid.NewString()[:8] })
	occupant := e.occupy(pool)
	run := e.issuerQueued(t, pool, f)
	e.free(occupant)
	got := e.waitFailed(run.ID)
	if got.StatusDetail != "queue: user_type_unknown" || got.FailureHint != queueUserTypeUnknownHint {
		t.Errorf("a run whose portal type is gone = %s %q %q", got.State, got.StatusDetail, got.FailureHint)
	}
}

// A 0.10.1 worker decodes the grant of a queue authority into a uuid and ends the run on the error; the
// portal-encoded grant never decodes there, and a console grant still does. The new decode reads both.
func TestPG_QueuePortalIssuerLaunch_OldDecodeEndsRun(t *testing.T) {
	grant := uuid.New()
	var old struct {
		Grant *uuid.UUID `json:"grant"`
	}
	issuer, _ := json.Marshal(queueAuthority{Sub: "delegate:person:aaaaaaaaaaaaaaaaaaaaaaaaaa:x", Grant: &queueGrant{ID: grant, Portal: true}})
	if err := json.Unmarshal(issuer, &old); err == nil {
		t.Fatalf("the old decode accepted an issuer-portal authority: %s", issuer)
	}
	console, _ := json.Marshal(queueAuthority{Sub: "alice", Grant: &queueGrant{ID: grant}})
	if err := json.Unmarshal(console, &old); err != nil || old.Grant == nil || *old.Grant != grant {
		t.Fatalf("the old decode of a console authority = %v %v, want the grant", old.Grant, err)
	}
	var now queueAuthority
	if err := json.Unmarshal(issuer, &now); err != nil || now.Grant == nil || now.Grant.ID != grant || !now.Grant.Portal || !now.portalIssuerLaunch() {
		t.Fatalf("the new decode = %+v %v, want the portal grant", now.Grant, err)
	}
}

// The start reads the portal's row, never the grant: an issuer-portal launch that waited past every grant's end
// still starts, and one whose portal was revoked meanwhile ends queue_portal_revoked.
func TestPortalDisable_QueuedRunStillStarts_PG(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 2)
	f := e.issuerPortal(t, nil)
	first, second := e.occupy(pool), e.occupy(pool)
	live, revoked := e.issuerQueued(t, pool, f), e.issuerQueued(t, pool, f)
	e.exec(`UPDATE run_queue SET queued_at = queued_at - interval '11 minutes' WHERE run_id = ANY($1)`, []uuid.UUID{live.ID, revoked.ID})
	e.free(first)
	e.waitStarted(live.ID)
	if _, _, err := e.pg.RevokeDelegate(context.Background(), f.portal.ID, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	e.free(second)
	if got := e.waitFailed(revoked.ID); got.StatusDetail != "queue: queue_portal_revoked" {
		t.Errorf("a run whose portal was revoked = %s %q", got.State, got.StatusDetail)
	}
}

// The start checks the run's unit against the pin on the tree as it stands: two runs wait in children of the pin,
// one child is reparented out from under the pin while they wait, and that run ends portal_unit_scope while the
// run in the child that stayed starts.
func TestPortalPin_QueuedStartRechecksCurrentTree_PG(t *testing.T) {
	e := newQueueEnv(t)
	ctx := context.Background()
	pool := e.cappedPool("Busy", 2)
	root := func(name string) types.GovernanceUnit {
		u, err := e.pg.UpsertGovernanceUnit(ctx, types.GovernanceUnit{ID: uuid.New(), Name: name, CreatedBy: "seed",
			Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2}})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	child := func(name string, parent uuid.UUID, id uuid.UUID) types.GovernanceUnit {
		u, err := e.pg.UpsertGovernanceUnit(ctx, types.GovernanceUnit{ID: id, Name: name, ParentID: &parent, Overlay: &types.CeilingOverlay{}, CreatedBy: "seed"})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	pin, outside := root("pin"), root("outside")
	stayer, mover := child("stayer", pin.ID, uuid.New()), child("mover", pin.ID, uuid.New())
	f := e.issuerPortal(t, &pin.ID)
	first, second := e.occupy(pool), e.occupy(pool)
	stays, moved := e.issuerQueued(t, pool, f), e.issuerQueued(t, pool, f)
	e.exec(`UPDATE agent_runs SET unit_id = $2 WHERE id = $1`, stays.ID, stayer.ID)
	e.exec(`UPDATE agent_runs SET unit_id = $2 WHERE id = $1`, moved.ID, mover.ID)
	// Both are in the pin as queued; the tree changes during the wait.
	child("mover", outside.ID, mover.ID)
	e.free(first)
	e.free(second)
	e.waitStarted(stays.ID)
	if got := e.waitFailed(moved.ID); got.StatusDetail != "queue: portal_unit_scope" || got.FailureHint != queuePortalUnitScopeHint {
		t.Errorf("a run whose unit was reparented out of the pin = %s %q %q", got.State, got.StatusDetail, got.FailureHint)
	}
}
