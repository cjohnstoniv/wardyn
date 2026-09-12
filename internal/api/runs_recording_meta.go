// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recordingMetaTailBytes bounds how much of a cast's TAIL StatAndTail reads to
// find the last output event's timestamp (R4-F077). asciicast output events
// are one PTY chunk each — typically well under a kilobyte — so this comfortably
// holds several trailing events without ever approaching the 64 MiB cap a cast
// itself is bounded to.
//
// ponytail: fixed window, not "read until a full line parses". A pathological
// single event near the very end larger than this (a huge single burst with no
// later event) makes the duration silently 0 instead of erroring — the same
// "best effort, never fail the list" tradeoff RecordingBytes/HasRecording make
// on any store error. Raise the window if that shows up in practice.
const recordingMetaTailBytes = 8 << 10 // 8 KiB

// wantsRecordingMeta reports whether the caller opted into the projection
// below via ?include=recording_meta. GATED, not default-on: handleListRuns
// backs both the Runs board (runs.tsx, POLL_MS=3000, limit=1000) and the
// Recordings screen, and a sequential StatAndTail per run in that response —
// measured ~7ms per 4 MiB cast, PG's substring/octet_length detoasting the
// whole bytea either way — turns a 3s poll of 1000 runs into up to ~7s of
// added latency for a board that renders none of these three fields. Only the
// Recordings screen, which actually shows them, passes the flag.
func wantsRecordingMeta(r *http.Request) bool {
	return r.URL.Query().Get("include") == "recording_meta"
}

// projectRecordingMeta fills HasRecording/RecordingBytes/RecordingDurationSec
// on each run from RecordingStore.StatAndTail, so the Recordings library can be
// built from a run listing alone instead of downloading every run's cast
// (F077-followup-server-probe: 39.8 MB measured for 200 runs). Best-effort: a
// store error or ErrNotFound just leaves the run's three fields at zero/false,
// the same as "no recording" — a metadata miss must never fail the run list.
// No-op unless wantsRecordingMeta(r) — see its doc for why this is opt-in.
func (s *Server) projectRecordingMeta(r *http.Request, runs []types.AgentRun) {
	if s.cfg.RecordingStore == nil || !wantsRecordingMeta(r) {
		return
	}
	ctx := r.Context()
	for i := range runs {
		size, tail, err := s.cfg.RecordingStore.StatAndTail(ctx, runs[i].ID.String(), recordingMetaTailBytes)
		if err != nil {
			// ErrNotFound is the ordinary "no recording" case. Any other error
			// (a store outage) is not logged per-run either — that would flood
			// the log once per run in the list — every OTHER field on this run
			// is still correct, and the console degrades to "no recording".
			continue
		}
		runs[i].HasRecording = true
		runs[i].RecordingBytes = size
		if d, ok := recording.LastOutputElapsed(tail); ok {
			runs[i].RecordingDurationSec = d
		}
	}
}
