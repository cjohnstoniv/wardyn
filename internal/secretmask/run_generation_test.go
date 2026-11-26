// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// twoRuns is a registry holding one value for each of runs a and b, with both Maskers built.
func twoRuns(t *testing.T) (r *Registry, a, b uuid.UUID) {
	t.Helper()
	r = NewRegistry()
	a, b = uuid.New(), uuid.New()
	if !r.AddLocal(a, []byte("run-a-first-value")) || !r.AddLocal(b, []byte("run-b-first-value")) {
		t.Fatal("AddLocal refused a new value")
	}
	_, _ = r.Masker(a), r.Masker(b)
	return r, a, b
}

// sameMasker reports whether m is backed by the same corpus array as before, i.e. was not rebuilt.
func sameMasker(before, m Masker) bool {
	bs, ms := before.Secrets(), m.Secrets()
	return len(bs) > 0 && len(ms) > 0 && &bs[0][0] == &ms[0][0]
}

func masks(m Masker, v string) bool {
	return string(m.Mask([]byte("x "+v+" y"))) != "x "+v+" y"
}

func TestAddLocalMovesOnlyItsRun(t *testing.T) {
	r, a, b := twoRuns(t)
	mb, gb, ga := r.Masker(b), r.RunGeneration(b), r.RunGeneration(a)

	r.AddLocal(a, []byte("run-a-second-value"))

	if !sameMasker(mb, r.Masker(b)) || r.RunGeneration(b) != gb {
		t.Error("a value added to run a rebuilt run b's Masker or moved its generation")
	}
	if r.RunGeneration(a) == ga {
		t.Error("a value added to run a did not move run a's generation")
	}
	if !masks(r.Masker(a), "run-a-second-value") {
		t.Error("run a's Masker does not mask its new value")
	}
	if masks(r.Masker(b), "run-a-second-value") {
		t.Error("run b's Masker masks run a's value")
	}
}

func TestRemoveLocalMovesOnlyItsRun(t *testing.T) {
	for _, drop := range []struct {
		name string
		fn   func(r *Registry, id uuid.UUID, v []byte)
	}{
		{"RemoveLocal", (*Registry).RemoveLocal},
		{"DropRunValue", (*Registry).DropRunValue},
	} {
		t.Run(drop.name, func(t *testing.T) {
			r, a, b := twoRuns(t)
			r.AddLocal(a, []byte("run-a-second-value"))
			_ = r.Masker(a)
			mb, gb, ga := r.Masker(b), r.RunGeneration(b), r.RunGeneration(a)

			drop.fn(r, a, []byte("run-a-second-value"))

			if !sameMasker(mb, r.Masker(b)) || r.RunGeneration(b) != gb {
				t.Error("a value removed from run a rebuilt run b's Masker or moved its generation")
			}
			if r.RunGeneration(a) == ga {
				t.Error("a value removed from run a did not move run a's generation")
			}
			if masks(r.Masker(a), "run-a-second-value") || !masks(r.Masker(a), "run-a-first-value") {
				t.Error("run a's Masker does not reflect the removal")
			}
		})
	}
}

func TestEvictMovesOnlyItsRunAndIsBounded(t *testing.T) {
	r, a, b := twoRuns(t)
	mb, gb, ga := r.Masker(b), r.RunGeneration(b), r.RunGeneration(a)

	r.Evict(a)

	if !sameMasker(mb, r.Masker(b)) || r.RunGeneration(b) != gb {
		t.Error("evicting run a rebuilt run b's Masker or moved its generation")
	}
	if r.RunGeneration(a) == ga {
		t.Error("evicting run a did not move run a's generation")
	}

	t.Run("evictions", func(t *testing.T) {
		r, _, b := twoRuns(t)
		boundedTombstones(t, r, b, func(id uuid.UUID, v []byte) { r.Evict(id) })
	})
	t.Run("every value removed", func(t *testing.T) {
		r, _, b := twoRuns(t)
		boundedTombstones(t, r, b, func(id uuid.UUID, v []byte) { r.RemoveLocal(id, v) })
	})
}

// boundedTombstones empties maxGenTombstones+1 runs with end: the generation map stays within the live runs plus
// maxGenTombstones throughout, and the emptying past the bound moves every run's generation, live's included.
func boundedTombstones(t *testing.T, r *Registry, live uuid.UUID, end func(id uuid.UUID, v []byte)) {
	t.Helper()
	const liveRuns = 2 // twoRuns
	for i := range maxGenTombstones + 1 {
		id, v := uuid.New(), []byte(fmt.Sprintf("short-lived-run-value-%d", i))
		r.AddLocal(id, v)
		before := r.RunGeneration(live)
		end(id, v)
		r.mu.RLock()
		n := len(r.run)
		r.mu.RUnlock()
		if n > liveRuns+maxGenTombstones {
			t.Fatalf("after %d emptied runs the generation map holds %d entries, want <= %d", i+1, n, liveRuns+maxGenTombstones)
		}
		moved := r.RunGeneration(live) != before
		if i < maxGenTombstones && moved {
			t.Fatalf("emptying run %d moved another run's generation before the bound", i+1)
		}
		if i == maxGenTombstones && !moved {
			t.Fatalf("emptying run %d, past the bound, did not move every run's generation", i+1)
		}
	}
}

func TestGlobalChangeMovesEveryRun(t *testing.T) {
	r, a, b := twoRuns(t)
	ma, mb, ga, gb := r.Masker(a), r.Masker(b), r.RunGeneration(a), r.RunGeneration(b)

	if err := r.AddGlobal(0, "", "test-credential", time.Now(), []byte("a-process-global-secret")); err != nil {
		t.Fatal(err)
	}

	if r.RunGeneration(a) == ga || r.RunGeneration(b) == gb {
		t.Error("a global change did not move every run's generation")
	}
	if sameMasker(ma, r.Masker(a)) || sameMasker(mb, r.Masker(b)) {
		t.Error("a global change did not rebuild every run's Masker")
	}
	if !masks(r.Masker(a), "a-process-global-secret") || !masks(r.Masker(b), "a-process-global-secret") {
		t.Error("a run's Masker does not mask the new global")
	}
}

func TestRunGenerationNeverRepeats(t *testing.T) {
	r := NewRegistry()
	runs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	last := map[uuid.UUID]uint64{}
	for _, id := range runs {
		last[id] = r.RunGeneration(id)
	}
	step := func(name string, touched map[uuid.UUID]bool, fn func()) {
		t.Helper()
		fn()
		for _, id := range runs {
			g := r.RunGeneration(id)
			switch {
			case touched[id] && g <= last[id]:
				t.Fatalf("%s: run %v generation %d, want above %d", name, id, g, last[id])
			case !touched[id] && g != last[id]:
				t.Fatalf("%s: untouched run %v generation moved %d -> %d", name, id, last[id], g)
			}
			last[id] = g
		}
	}
	all := map[uuid.UUID]bool{runs[0]: true, runs[1]: true, runs[2]: true}
	only := func(id uuid.UUID) map[uuid.UUID]bool { return map[uuid.UUID]bool{id: true} }
	glob := 0
	global := func() {
		glob++
		if err := r.AddGlobal(0, "", "test-credential", time.Now(), []byte(fmt.Sprintf("global-secret-value-%d", glob))); err != nil {
			t.Fatal(err)
		}
	}

	step("run 0 add", only(runs[0]), func() { r.AddLocal(runs[0], []byte("run-zero-value-1")) })
	step("run 1 add", only(runs[1]), func() { r.AddLocal(runs[1], []byte("run-one-value-1")) })
	step("global", all, global)
	step("run 0 add again", only(runs[0]), func() { r.AddLocal(runs[0], []byte("run-zero-value-2")) })
	step("run 2 add", only(runs[2]), func() { r.AddLocal(runs[2], []byte("run-two-value-1")) })
	step("run 0 remove", only(runs[0]), func() { r.RemoveLocal(runs[0], []byte("run-zero-value-2")) })
	step("run 1 evict", only(runs[1]), func() { r.Evict(runs[1]) })
	step("run 1 evict again (nothing held)", nil, func() { r.Evict(runs[1]) })
	step("global again", all, global)
	step("run 1 add after evict", only(runs[1]), func() { r.AddLocal(runs[1], []byte("run-one-value-2")) })
	step("run 0 remove last", only(runs[0]), func() { r.RemoveLocal(runs[0], []byte("run-zero-value-1")) })
	step("global third", all, global)
}

// rebuildsUnderAnotherRunsAdds adds one fresh value to run a, then asks for the Masker of each of the others, and
// returns how many of the others' cached Maskers were rebuilt. Base APIs only, so it also measures the registry
// before generations were kept per run.
func rebuildsUnderAnotherRunsAdds(r *Registry, a uuid.UUID, others []uuid.UUID, i int) int {
	before := make([]*runMaskers, len(others))
	r.mu.RLock()
	for j, id := range others {
		before[j] = r.cached[id]
	}
	r.mu.RUnlock()
	r.AddLocal(a, []byte(fmt.Sprintf("run-a-fresh-value-%d", i)))
	n := 0
	for j, id := range others {
		_ = r.Masker(id)
		r.mu.RLock()
		if r.cached[id] != before[j] {
			n++
		}
		r.mu.RUnlock()
	}
	return n
}

func hundredRuns() (*Registry, uuid.UUID, []uuid.UUID) {
	r := NewRegistry()
	a := uuid.New()
	others := make([]uuid.UUID, 99)
	for j := range others {
		others[j] = uuid.New()
		r.AddLocal(others[j], []byte(fmt.Sprintf("other-run-value-%d", j)))
		_ = r.Masker(others[j])
	}
	return r, a, others
}

func BenchmarkMaskerRebuildsUnderAnotherRunsAdds(b *testing.B) {
	r, a, others := hundredRuns()
	total := 0
	for i := 0; b.Loop(); i++ {
		total += rebuildsUnderAnotherRunsAdds(r, a, others, i)
	}
	b.ReportMetric(float64(total)/float64(b.N), "rebuilds/op")
}

func TestAnotherRunsAddRebuildsNoMasker(t *testing.T) {
	r, a, others := hundredRuns()
	for i := range 20 {
		if n := rebuildsUnderAnotherRunsAdds(r, a, others, i); n != 0 {
			t.Fatalf("add %d to run a rebuilt %d other runs' Maskers, want 0", i, n)
		}
	}
}
