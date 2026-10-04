// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

type fakeMaskSync struct{ err error }

func (f fakeMaskSync) Synced(context.Context) error { return f.err }

func haRows(t *testing.T, srv *Server) map[string]SetupCheck {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	code, st := decodeSetup(t, srv, adminToken)
	if code != 200 {
		t.Fatalf("GET /setup/status: code = %d", code)
	}
	rows := map[string]SetupCheck{}
	for _, c := range st.Checks {
		rows[c.ID] = c
	}
	return rows
}

// The three rows, their order and their sentences are mock packet M10's canon,
// pinned byte for byte.
func TestSetupStatus_HARowsCarryTheApprovedCopy(t *testing.T) {
	srv := New(Config{AdminToken: adminToken, HA: true, MaskSync: fakeMaskSync{}, RecordingStore: &recording.FSStore{}})
	rows := haRows(t, srv)
	want := []SetupCheck{
		{ID: "ha_mode", Label: "High availability", Status: "ok",
			Detail: "High availability is on (`WARDYN_HA`). Several replicas serve this deployment. Per-replica limits apply to each replica, so they add up across replicas."},
		{ID: "recording_store_shared", Label: "Shared recording store", Status: "ok",
			Detail: "Recordings are stored in Postgres, so every replica reads the same ones."},
		{ID: "mask_registry_shared", Label: "Shared secret masking", Status: "ok",
			Detail: "This replica has the latest secret-masking list that every replica shares."},
	}
	for _, w := range want {
		if got := rows[w.ID]; got != w {
			t.Errorf("row %s = %+v, want %+v", w.ID, got, w)
		}
	}
}

func TestSetupStatus_RecordingOffIsInfo(t *testing.T) {
	srv := New(Config{AdminToken: adminToken, HA: true, MaskSync: fakeMaskSync{}})
	got := haRows(t, srv)["recording_store_shared"]
	want := SetupCheck{ID: "recording_store_shared", Label: "Shared recording store", Status: "info",
		Detail: "Recording is off, so no replica stores recordings."}
	if got != want {
		t.Errorf("row = %+v, want %+v", got, want)
	}
}

// A replica that cannot confirm its masking list fails the row, with the
// approved sentence and fix, and never blocks the console.
func TestSetupStatus_MaskRegistrySharedFailsWhenTheReplicaCannotSync(t *testing.T) {
	for name, probe := range map[string]MaskSyncProbe{
		"postgres unreachable": fakeMaskSync{err: errors.New("connection refused")},
		"no registry":          nil,
	} {
		t.Run(name, func(t *testing.T) {
			got := haRows(t, New(Config{AdminToken: adminToken, HA: true, MaskSync: probe}))["mask_registry_shared"]
			want := SetupCheck{ID: "mask_registry_shared", Label: "Shared secret masking", Status: "fail",
				Detail: "This replica can't confirm it has the latest secret-masking list. Until it can, it refuses recording uploads and new attaches, and shows a placeholder in place of live output.",
				Fix:    "Check this replica's connection to Postgres. It catches up on its own when the connection returns."}
			if got != want {
				t.Errorf("row = %+v, want %+v", got, want)
			}
			if got.Blocking {
				t.Error("the row blocks the console")
			}
		})
	}
}

// Without WARDYN_HA the rows do not exist.
func TestSetupStatus_NoHARowsWithoutHA(t *testing.T) {
	rows := haRows(t, New(Config{AdminToken: adminToken, MaskSync: fakeMaskSync{}}))
	for _, id := range []string{"ha_mode", "recording_store_shared", "mask_registry_shared"} {
		if _, ok := rows[id]; ok {
			t.Errorf("row %s present without HA", id)
		}
	}
}
