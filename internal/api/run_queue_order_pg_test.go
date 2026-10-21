// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run queued under a session that recorded a revocation sequence keeps that sequence, and a revoke made
// while it waits stops its start though the revoke's time reads before the moment it was queued (the wall
// clock stepped back between the two).
func TestPG_QueueWaitingRunIsOrderedByTheLaunchingSessionsSequence(t *testing.T) {
	e := newQueueEnv(t)
	rev := e.revocations()
	pool := e.cappedPool("Busy", 1)
	seq, err := rev.RevocationSeq(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(oidc.Session{V: oidc.SessionCodecVersion, Sub: e.personP, Email: "person@corp.example",
		Role: oidc.RoleUser, UserType: types.UserTypeStandard, Expiry: time.Now().UTC().Add(time.Hour), IssuedAt: time.Now().UTC(),
		RevocationSeq: seq})
	if err != nil {
		t.Fatal(err)
	}
	occupant := e.occupy(pool)
	waiting := e.run(e.queuedLaunch(signedSessionCookie(payload), pool, "").ID)
	var auth queueAuthority
	if err := e.pool.QueryRow(context.Background(), `SELECT authority FROM run_queue WHERE run_id = $1`, waiting.ID).Scan(&auth); err != nil {
		t.Fatal(err)
	}
	if auth.Seq != seq {
		t.Fatalf("the queue row recorded sequence %d, want the launching session's %d", auth.Seq, seq)
	}
	e.exec(`INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ($1, clock_timestamp() - interval '2 seconds')`, e.personP)
	e.free(occupant)
	if got := e.waitFailed(waiting.ID); got.StatusDetail != "queue: "+string(types.RunEndIdentityDeactivated) || got.SPIFFEID != "" {
		t.Errorf("the run ended %q with spiffe %q; want identity_deactivated and nothing minted", got.StatusDetail, got.SPIFFEID)
	}
}

// A service identity's launch keeps its token's revocation sequence, and a revoke of its SPONSOR made
// while the run waits stops the start though the revoke's time reads before the moment it was queued.
func TestPG_QueueWaitingServiceRunIsOrderedByItsTokensSequence(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t)
	e.revocations()
	pool := e.cappedPool("Busy", 1)
	if _, err := e.pg.IssueLoginIdentity(ctx, store.LoginIdentity{Principal: e.personP, Issuer: "https://idp.example", TenantID: "t", ObjectID: "o-1", Email: "person@corp.example"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	si, err := e.pg.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: "ci-queue", Sponsor: e.personP, UserType: types.UserTypeStandard, CreatedBy: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	ageSponsorships(t, e.pg.Pool) // see svcEnv.mintReq
	w := e.call(e.person, http.MethodPost, "/api/v1/me/service-identities/"+si.Name+"/tokens", `{"name":"ci","ttl_seconds":3600}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint the service token = %d %s", w.Code, w.Body)
	}
	tok := decodeToken(t, w)
	seq := tokenRevSeq(t, e.pool, tok.ID)
	if seq <= 0 {
		t.Fatalf("service token rev_seq = %d, want the sequence at mint", seq)
	}

	occupant := e.occupy(pool)
	w = do(t, e.srv, http.MethodPost, "/api/v1/runs", tok.Token, endBody("background", `"runner_pool_id":"`+pool.ID.String()+`"`))
	var out queuedLaunchAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusCreated || out.State != types.RunQueued {
		t.Fatalf("the service launch into a full pool = %d %s; want 201 QUEUED", w.Code, w.Body)
	}
	var auth queueAuthority
	if err := e.pool.QueryRow(ctx, `SELECT authority FROM run_queue WHERE run_id = $1`, out.ID).Scan(&auth); err != nil {
		t.Fatal(err)
	}
	if auth.Service != si.Name || auth.Sponsor != e.personP || auth.Seq != seq {
		t.Fatalf("the queue row recorded service %q, sponsor %q, sequence %d; want %q, %q and the token's %d", auth.Service, auth.Sponsor, auth.Seq, si.Name, e.personP, seq)
	}
	e.exec(`INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ($1, clock_timestamp() - interval '1 hour')`, e.personP)
	e.free(occupant)
	if got := e.waitFailed(out.ID); got.StatusDetail != "queue: "+string(types.RunEndIdentityDeactivated) || got.SPIFFEID != "" {
		t.Errorf("the service run ended %q with spiffe %q; want identity_deactivated and nothing minted", got.StatusDetail, got.SPIFFEID)
	}
}

// A queue row written by a service token from before the sequence existed carries none, and its sponsor's
// check falls back to the time the run was queued: a cut that reads later than that still stops the start,
// one that reads earlier does not.
func TestPG_QueueServiceRunWithoutASequenceIsOrderedByItsQueueTime(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t)
	e.revocations()
	if _, err := e.pg.IssueLoginIdentity(ctx, store.LoginIdentity{Principal: e.personP, Issuer: "https://idp.example", TenantID: "t", ObjectID: "o-1", Email: "person@corp.example"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	si, err := e.pg.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: "ci-queue", Sponsor: e.personP, UserType: types.UserTypeStandard, CreatedBy: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	auth := queueAuthority{Sub: si.Principal(), Role: oidc.RoleUser, UserType: types.UserTypeStandard, Groups: []string{}, Service: si.Name, Sponsor: e.personP}
	e.exec(`INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ($1, clock_timestamp() - interval '1 hour')`, e.personP)
	if end := e.srv.queueOwnerStanding(e.startOf(auth, types.AgentRun{ID: uuid.New()})); end != nil {
		t.Errorf("a cut an hour before the queue time ended a start with no sequence: %+v", end)
	}
	e.exec(`UPDATE oidc_session_revocations SET revoked_at = clock_timestamp() + interval '1 hour' WHERE sub = $1`, e.personP)
	if end := e.srv.queueOwnerStanding(e.startOf(auth, types.AgentRun{ID: uuid.New()})); end == nil || end.token != string(types.RunEndIdentityDeactivated) {
		t.Errorf("a cut after the queue time left a start with no sequence standing: %+v", end)
	}
}
