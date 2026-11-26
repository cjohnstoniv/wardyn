// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import "github.com/google/uuid"

// generations is the Registry's Masker cache key, split per run so a change to one run's values leaves every other
// run's cached Maskers, and its Watch, alone (#4069). Every bump takes the next seq, so a run's generation,
// max(gen, run[id]), only ever grows: a value read before a change never equals one read after it.
type generations struct {
	gen        uint64               // the last change to the globals, which every run's corpus holds
	seq        uint64               // the last value handed out
	run        map[uuid.UUID]uint64 // the last change to one run's own values since gen moved
	tombstones int                  // entries in run left by evicted runs
}

// maxGenTombstones bounds the entries evicted runs leave in run: past it an eviction moves gen instead, which
// clears them all, so the map holds at most the live runs plus this many.
const maxGenTombstones = 1024

func (g *generations) bumpGlobal() {
	g.seq++
	g.gen = g.seq
	clear(g.run) // every entry is below gen now, so max(gen, run[id]) is gen for all
	g.tombstones = 0
}

func (g *generations) bumpRun(id uuid.UUID) {
	g.seq++
	if g.run == nil {
		g.run = map[uuid.UUID]uint64{}
	}
	g.run[id] = g.seq
}

// bumpEvicted moves an evicted run's generation. The entry stays, so a caller holding the run's old generation
// sees it move; the count of such entries is bounded by maxGenTombstones.
func (g *generations) bumpEvicted(id uuid.UUID) {
	if g.tombstones >= maxGenTombstones {
		g.bumpGlobal()
		return
	}
	g.tombstones++
	g.bumpRun(id)
}

func (g *generations) runGen(id uuid.UUID) uint64 { return max(g.gen, g.run[id]) }
