// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskstore

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// A registration held after its commit while the run's values are erased (the fence, then the owner's
// tombstones, which this replica applies before the registration caches anything) or purged ends with
// no copy of the value in this process: not in the registry and not in the store's own row refs, and a
// later read brings none back. Catches a registration that caches the value after it returns, past a
// tombstone this replica has already applied and will never read again.
func TestPG_MaskErasureARegistrationHeldAcrossAnEraseLeavesNoCacheCopy(t *testing.T) {
	for _, how := range []string{"erase", "purge"} {
		t.Run(how, func(t *testing.T) {
			pool, keys := erasurePG(t)
			reg := secretmask.NewRegistry()
			st := New(pool, keys, reg)
			ctx := t.Context()
			runID := uuid.New()
			if _, err := pool.Exec(ctx, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
				VALUES ($1, 'sub', 'claude-code', 'a/b', 'CC2', 'RUNNING', 'spiffe://x/' || $2, 'docker')`, runID, runID.String()); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO run_mask_manifest (run_id, owner) VALUES ($1, 'alice')`, runID); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			afterRunCommit = func() { close(entered); <-release }
			t.Cleanup(func() { afterRunCommit = func() {} })
			value := []byte("registered-across-the-" + how)
			done := make(chan error, 1)
			go func() { done <- reg.Add(runID, value) }()
			erasureEntered(t, entered)
			if how == "erase" {
				if _, err := pool.Exec(ctx, `UPDATE run_mask_manifest SET fenced_at = now() WHERE run_id = $1`, runID); err != nil {
					t.Fatal(err)
				}
				if _, err := st.EraseOwner(ctx, "alice"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := st.PurgeRuns(ctx, []uuid.UUID{runID}); err != nil {
					t.Fatal(err)
				}
				if err := st.Fresh(ctx, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			if err := erasureAwait(t, done); !errors.Is(err, ErrFenced) {
				t.Errorf("the held registration = %v, want ErrFenced", err)
			}
			check := func(when string) {
				for _, v := range reg.Snapshot(runID) {
					if bytes.Equal(v, value) {
						t.Errorf("%s: the registry still holds the value", when)
					}
				}
				st.sync.mu.Lock()
				defer st.sync.mu.Unlock()
				for _, rf := range st.sync.rows {
					if rf.runID == runID {
						t.Errorf("%s: the store still holds a ref with the run's value", when)
					}
				}
			}
			check("after the registration returned")
			if err := st.Fresh(ctx, time.Now()); err != nil {
				t.Fatal(err)
			}
			check("after a later read")
		})
	}
}
