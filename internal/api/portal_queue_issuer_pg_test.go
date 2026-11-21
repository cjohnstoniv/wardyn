// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// roleSpyStore counts the console role-mapping reads a start makes.
type roleSpyStore struct {
	store.Store
	roleReads atomic.Int64
}

func (s *roleSpyStore) ListRoleMappings(ctx context.Context) ([]types.RoleMapping, error) {
	s.roleReads.Add(1)
	return s.Store.ListRoleMappings(ctx)
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
		run.ID, f.principal, types.UserTypeStandard, f.portal.ID.String(), uuid.NewString())
	return run
}

// A queued issuer-portal launch starts as role user under the portal row's own user type: the console's role
// mappings are never read, whatever they would derive for the empty email and groups.
func TestPG_QueuePortalIssuerLaunch_NoConsoleRoleRead(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	f := e.issuerPortal(t, nil)
	occupant := e.occupy(pool)
	run := e.issuerQueued(t, pool, f)
	spy := &roleSpyStore{Store: e.srv.cfg.Store}
	e.srv.cfg.Store = spy
	e.free(occupant)
	started := e.waitStarted(run.ID)
	if n := spy.roleReads.Load(); n != 0 {
		t.Errorf("the start read the console role mappings %d times, want none", n)
	}
	rows := e.audits(started.ID, "run.queue.start")
	if len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("run.queue.start rows = %+v, want one success", rows)
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

// The start checks the run's unit against the pin on the tree as it stands: a run whose unit was moved out of
// the pin while it waited ends portal_unit_scope, and one still under it starts.
func TestPortalPin_QueuedStartRechecksCurrentTree_PG(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 2)
	mk := func(name string, parent *uuid.UUID) types.GovernanceUnit {
		u, err := e.pg.UpsertGovernanceUnit(context.Background(), types.GovernanceUnit{ID: uuid.New(), Name: name, ParentID: parent, CreatedBy: "seed",
			Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2}})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	pin := mk("pin", nil)
	inside, outside := mk("inside", &pin.ID), mk("outside", nil)
	f := e.issuerPortal(t, &pin.ID)
	first, second := e.occupy(pool), e.occupy(pool)
	stays, moved := e.issuerQueued(t, pool, f), e.issuerQueued(t, pool, f)
	e.exec(`UPDATE agent_runs SET unit_id = $2 WHERE id = $1`, stays.ID, inside.ID)
	e.exec(`UPDATE agent_runs SET unit_id = $2 WHERE id = $1`, moved.ID, outside.ID)
	e.free(first)
	e.free(second)
	e.waitStarted(stays.ID)
	if got := e.waitFailed(moved.ID); got.StatusDetail != "queue: portal_unit_scope" || got.FailureHint != queuePortalUnitScopeHint {
		t.Errorf("a run moved out of the pin = %s %q %q", got.State, got.StatusDetail, got.FailureHint)
	}
}
