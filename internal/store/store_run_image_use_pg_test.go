// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reuseTestRef is a reuse tag with a random key, so the shared database's other rows cannot match it.
func reuseTestRef(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "wardyn-byoi/reuse:" + hex.EncodeToString(b)
}

func TestPG_RunImageUse_ReportsTheNewestRunAndWhetherOneIsLive(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPool(t)
	day := 24 * time.Hour
	now := time.Now().UTC()
	refA, refB, refC, refD, refE, refF := reuseTestRef(t), reuseTestRef(t), reuseTestRef(t), reuseTestRef(t), reuseTestRef(t), reuseTestRef(t)
	add := func(state types.RunState, ref string, age time.Duration, sandbox string) types.AgentRun {
		r := newRun(state)
		r.Image, r.SandboxRef = ref, sandbox
		r.CreatedAt, r.UpdatedAt = now.Add(-age), now.Add(-age)
		return persistRun(t, ctx, pool, r)
	}
	add(types.RunCompleted, refA, 10*day, "")
	newest := add(types.RunCompleted, refA, 3*day, "")
	add(types.RunRunning, refB, time.Hour, "")
	add(types.RunStopped, refC, time.Hour, "wardyn-agent-x")
	add(types.RunQueued, refD, time.Hour, "")
	add(types.RunFailed, refE, 9*day, "")

	rows, err := store.NewPG(pool).RunImageUse(ctx, []string{refA, refB, refC, refD, refE, refF})
	if err != nil {
		t.Fatalf("RunImageUse: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5 (none for a ref no run records): %+v", len(rows), rows)
	}
	got := map[string]store.RunImageUse{}
	for _, u := range rows {
		got[u.Ref] = u
	}
	if u := got[refA]; u.InUse || u.LastCreated.Sub(newest.CreatedAt).Abs() > time.Second {
		t.Errorf("A = %+v, want not in use and last created at %v", u, newest.CreatedAt)
	}
	for name, ref := range map[string]string{"B (running)": refB, "C (kept sandbox)": refC, "D (queued)": refD} {
		if !got[ref].InUse {
			t.Errorf("%s: InUse = false, want true", name)
		}
	}
	if got[refE].InUse {
		t.Errorf("E (failed): InUse = true, want false")
	}
	if _, ok := got[refF]; ok {
		t.Errorf("F has a row, want none")
	}
	empty, err := store.NewPG(pool).RunImageUse(ctx, nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("RunImageUse(nil) = %v, %v, want an empty non-nil slice and no error", empty, err)
	}
}
