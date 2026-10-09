// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

func erasureKeyContract(t *testing.T, s recording.Store) {
	t.Helper()
	for _, key := range []string{".", "..~suffix", "~suffix", "parent~attach~part", "percent%_", strings.Repeat("k", 240)} {
		t.Run(key, func(t *testing.T) {
			family, _, _ := strings.Cut(key, "~")
			other := family + "-distinct"
			for _, saved := range []string{key, key + "~x", other} {
				if err := s.SaveCast(t.Context(), saved, strings.NewReader("cast")); err != nil {
					t.Fatalf("save valid key %q: %v", saved, err)
				}
			}
			if n, err := s.(recording.RunDeleter).DeleteRun(t.Context(), key); err != nil || n != 2 {
				t.Fatalf("erase key family %q = %d, %v", key, n, err)
			}
			assertErased(t, s, key)
			assertErased(t, s, key+"~x")
			if err := s.SaveCastNamed(t.Context(), key, "late", strings.NewReader("late")); !errors.Is(err, recording.ErrErased) {
				t.Fatalf("late composite descendant = %v", err)
			}
			rc, err := s.OpenCast(t.Context(), other)
			if err != nil {
				t.Fatalf("erasure leaked into distinct family %q: %v", other, err)
			}
			_ = rc.Close()
			if strings.Contains(key, "~") {
				sibling := family + "~sibling"
				if err := s.SaveCast(t.Context(), sibling, strings.NewReader("sibling")); err != nil {
					t.Fatalf("composite erasure widened to sibling: %v", err)
				}
			}
		})
	}
}

func TestFSStore_ErasureKeyFamilies(t *testing.T) {
	s, _ := recording.NewFSStore(t.TempDir())
	erasureKeyContract(t, s)
}

func TestPGStore_ErasureKeyFamilies(t *testing.T) {
	pool := pgPool(t)
	// These are deliberately fixed pathological keys; clean just these test
	// families so rerunning against the same test database remains meaningful.
	for _, run := range []string{".", "..", "", "parent", "..~suffix", "~suffix", "parent~attach~part", "percent%_", strings.Repeat("k", 240)} {
		if _, err := pool.Exec(t.Context(), `DELETE FROM recording_erasures WHERE run_id = $1`, run); err != nil {
			t.Fatal(err)
		}
	}
	erasureKeyContract(t, recording.NewPGStore(pool))
}
