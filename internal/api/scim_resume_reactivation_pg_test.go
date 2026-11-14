// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// afterLeavers runs hook once, with what the sweeper's read of pending leavers returned, before the sweeper
// acts on it.
type afterLeavers struct {
	store.PG
	mu   sync.Mutex
	hook func([]store.PendingLeaver)
}

func (s *afterLeavers) PendingLeavers(ctx context.Context, idleFor time.Duration, limit int) ([]store.PendingLeaver, error) {
	out, err := s.PG.PendingLeavers(ctx, idleFor, limit)
	s.mu.Lock()
	hook := s.hook
	s.hook = nil
	s.mu.Unlock()
	if hook != nil {
		hook(out)
	}
	return out, err
}

// The sweeper lists a suspension that stopped part way, and the identity provider reactivates the person
// before the sweeper resumes it. The reactivation stands: the resume deactivates nobody, schedules no purge,
// bumps no epoch, revokes no token, kills no run and records no deactivation. Guarded by WARDYN_TEST_PG
// (newSCIMEnv).
func TestSCIMSweeperResumeLeavesAReactivationStanding(t *testing.T) {
	purge := func(c *Config) { c.SCIM.PurgeAfter = 48 * time.Hour }
	e := newSCIMEnv(t, purge)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	e.seedRun(purgeSub, types.RunRunning)
	e.a.runner.setFailKills(1000)
	if w := e.patch(e.a, f.id, patchOf("false")); w.Code != http.StatusInternalServerError {
		t.Fatalf("suspend with a failing runner = %d, want 5xx", w.Code)
	}
	e.a.runner.setFailKills(0)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE deprovision_jobs SET updated_at = now() - interval '1 hour' WHERE identity_id = $1`, f.id); err != nil {
		t.Fatal(err)
	}

	var epoch int64
	var run, tok uuid.UUID
	leavers := &afterLeavers{PG: e.st, hook: func(listed []store.PendingLeaver) {
		if !slices.ContainsFunc(listed, func(p store.PendingLeaver) bool { return p.ID.String() == f.id }) {
			t.Errorf("the sweeper did not list the stopped suspension: %+v", listed)
		}
		if w := e.patch(e.b, f.id, patchOf("true")); w.Code != http.StatusOK {
			t.Errorf("reactivate = %d %s", w.Code, w.Body.String())
		}
		epoch = e.identity(f.id).AuthorityEpoch
		run = e.seedRun(purgeSub, types.RunRunning)
		tok, _ = e.seedToken(purgeSub, purgeEmail)
	}}
	n := e.node(purge, func(c *Config) { c.Store = leavers })
	if err := n.srv.SweepSCIMPurge(ctx); err != nil {
		t.Errorf("sweep = %v", err)
	}
	if run == uuid.Nil {
		t.Fatal("the sweeper never read its pending leavers")
	}

	if row := e.identity(f.id); row.DeactivatedAt != nil || row.PurgeAfter != nil || row.AuthorityEpoch != epoch {
		t.Errorf("identity after the resume: deactivated %v, purge %v, epoch %d; want active, no purge, epoch %d",
			row.DeactivatedAt, row.PurgeAfter, row.AuthorityEpoch, epoch)
	}
	if state := e.runState(run); state != types.RunRunning || n.runner.killCount() != 0 {
		t.Errorf("the resume reached a run: state %s, %d kills", state, n.runner.killCount())
	}
	if e.tokenRevoked(tok) {
		t.Error("the resume revoked the reactivated person's token")
	}
	if rows := e.rows(n, "scim.user.deactivate"); len(rows) != 0 {
		t.Errorf("the resume wrote %d scim.user.deactivate rows, want none", len(rows))
	}
}
