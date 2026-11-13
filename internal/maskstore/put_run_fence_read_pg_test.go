// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskstore

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// A registration whose last read of the run's fence fails keeps the value it committed masked: a second
// registration of the value found it cached and returned, and this replica's read of the table moved past
// its row, so nothing would bring the value back. The registration reports the error; the value stays in
// the registry and the run's output is masked after a later read.
func TestPG_MaskARegistrationWhoseFenceReadFailsKeepsTheValue(t *testing.T) {
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
	value := []byte("registered-before-a-failed-fence-read")
	var again error
	beforeRunFenceRead = func(c context.Context) context.Context {
		again = reg.Add(runID, value)
		if err := st.Fresh(ctx, time.Now()); err != nil {
			t.Error(err)
		}
		failed, cancel := context.WithCancel(c)
		cancel()
		return failed
	}
	t.Cleanup(func() { beforeRunFenceRead = func(c context.Context) context.Context { return c } })
	if err := reg.Add(runID, value); err == nil || errors.Is(err, ErrFenced) {
		t.Fatalf("the registration whose fence read failed = %v, want the read's error", err)
	}
	if again != nil {
		t.Fatalf("the second registration = %v, want nil (the value was cached)", again)
	}
	if err := st.Fresh(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if out := reg.Masker(runID).Mask([]byte("token " + string(value))); bytes.Contains(out, value) {
		t.Errorf("the run's output still carries the value: %q", out)
	}
	st.sync.mu.Lock()
	defer st.sync.mu.Unlock()
	kept := false
	for _, rf := range st.sync.rows {
		kept = kept || rf.runID == runID
	}
	if !kept {
		t.Error("the store dropped its ref to the committed row, so a later tombstone would not drop the value")
	}
}
