// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner reports posture only on change, so an old session's late report must not overwrite the fresh
// session's: it writes nothing and reports no change (no audit row, no revoke). A runner that is not
// claimed is ErrNotFound, as for RecordRunnerPosture.
func TestPG_Runners_PostureFromASupersededSessionWritesNothing(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	r, err := st.CreateRunner(ctx, newRunner("posture-owner"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	restrictive := types.RunnerPosture{OS: "linux", DiskEncrypted: false, Evidence: types.PostureEvidenceOSProbe}
	permissive := types.RunnerPosture{OS: "linux", DiskEncrypted: true, Evidence: types.PostureEvidenceOSProbe}
	old, fresh := uuid.NewString(), uuid.NewString()
	if _, err := st.RecordRunnerPostureFrom(ctx, r.ID, old, permissive, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("posture from an unclaimed runner = %v, want ErrNotFound", err)
	}
	if _, err := st.ClaimRunner(ctx, r.ID, r.Owner, r.KeyFingerprint, now); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginRunnerInventory(ctx, r.ID, old); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginRunnerInventory(ctx, r.ID, fresh); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.RecordRunnerPostureFrom(ctx, r.ID, fresh, restrictive, now); err != nil || !changed {
		t.Fatalf("the fresh session's report: changed=%v err=%v", changed, err)
	}
	if changed, err := st.RecordRunnerPostureFrom(ctx, r.ID, old, permissive, now); err != nil || changed {
		t.Fatalf("the old session's report: changed=%v err=%v, want no change and no error", changed, err)
	}
	got, err := st.GetRunner(ctx, r.ID)
	if err != nil || got.Posture != restrictive {
		t.Fatalf("stored posture = %+v, %v; want the fresh session's %+v", got.Posture, err, restrictive)
	}
}
