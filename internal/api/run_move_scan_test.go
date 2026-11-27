// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMoveScan_UncoveredRunRefused: a run whose secret values are not all known here is never carried unscanned: the
// move fails move_scan_uncovered before anything is exported, and the destination's token is revoked.
func TestMoveScan_UncoveredRunRefused(t *testing.T) {
	f := newMoveFixture(t)
	pool, _ := miscCovClosedPool(t)
	f.srv.cfg.MaskManifests = maskmanifest.New(pool, nil, secretmask.NewRegistry())
	if code, body := f.move(t, "org"); code != http.StatusAccepted {
		t.Fatalf("move = %d %s, want 202", code, body)
	}
	mv := f.only(t)
	if mv.State != types.MoveFailed || mv.Failure != reasonMoveScanUncovered {
		t.Errorf("row = %s %q, want failed move_scan_uncovered", mv.State, mv.Failure)
	}
	if f.mr.exportCount() != 0 || f.mr.importCount() != 0 {
		t.Errorf("exports %d imports %d, want none", f.mr.exportCount(), f.mr.importCount())
	}
	if mv.Cleanup != types.MoveCleanupDone {
		t.Errorf("cleanup = %s, want done (the destination token revoked)", mv.Cleanup)
	}
	if f.moveRows("failure") != 1 {
		t.Errorf("run.move failure rows = %d, want 1", f.moveRows("failure"))
	}
}
