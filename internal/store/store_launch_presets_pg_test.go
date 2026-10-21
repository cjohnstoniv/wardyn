// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A preset deleted and recreated under its name resumes above the version it reached, so a client
// holding the old version is never matched to the new content.
func TestPG_LaunchPreset_VersionSurvivesDeleteAndRecreate(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := t.Context()
	put := func(desc string) types.LaunchPreset {
		t.Helper()
		got, _, err := st.PutLaunchPreset(ctx, types.LaunchPreset{Name: "ci", Description: desc, Request: json.RawMessage(`{}`), CreatedBy: "admin"})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := put("one"); got.Version != 1 {
		t.Fatalf("first write version = %d, want 1", got.Version)
	}
	if got := put("two"); got.Version != 2 {
		t.Fatalf("second write version = %d, want 2", got.Version)
	}
	if _, err := st.DeleteLaunchPreset(ctx, "ci"); err != nil {
		t.Fatal(err)
	}
	if got := put("three"); got.Version != 3 {
		t.Fatalf("recreated version = %d, want 3", got.Version)
	}
	if _, err := st.DeleteLaunchPreset(ctx, "ci"); err != nil {
		t.Fatal(err)
	}
	if got := put("four"); got.Version != 4 {
		t.Fatalf("recreated again version = %d, want 4", got.Version)
	}
	if _, err := st.DeleteLaunchPreset(ctx, "absent"); err == nil {
		t.Fatal("deleting an unknown preset did not report not found")
	}
}
