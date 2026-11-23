// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "time"

// debounceMapSoft is the size past which pruneStale sweeps a debounce or ready map, and debounceMapHard the size
// past which it clears one, the bound these maps had before.
const (
	debounceMapSoft = 4096
	debounceMapHard = 1 << 16
)

// pruneStale deletes the entries of m at least keep old, at most once per keep and only once m holds more than
// debounceMapSoft. Each map it runs on answers "due", "not fresh" or "not ready" for such an entry anyway, so
// deleting it changes no answer, where clearing the map made every live run due again. Past debounceMapHard it
// clears m. at reads an entry's time; swept is the map's own last-sweep time. Callers hold the map's lock.
func pruneStale[K comparable, V any](m map[K]V, at func(V) time.Time, keep time.Duration, now time.Time, swept *time.Time) {
	if len(m) <= debounceMapSoft {
		return
	}
	if len(m) > debounceMapHard {
		clear(m)
		*swept = now
		return
	}
	if now.Sub(*swept) < keep {
		return
	}
	*swept = now
	for k, v := range m {
		if now.Sub(at(v)) >= keep {
			delete(m, k)
		}
	}
}

// stampTime is pruneStale's at for a map of times.
func stampTime(t time.Time) time.Time { return t }
