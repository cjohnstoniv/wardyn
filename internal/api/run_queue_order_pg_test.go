// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
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
