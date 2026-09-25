/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #159 — Recordings-screen-only copy for server-side paging. Frozen strings
// from the approved console-0.8 prototype (surface 3, docs' mock round),
// shipped byte for byte. Kept out of wardyn/copy.ts on purpose: that file is
// near its own size cap (check-file-size.sh) and this vocabulary belongs to
// one screen, not the shared glossary.
//
// X-Wardyn-Truncated is a boolean and the server sends no total, so none of
// these may ever imply a known count of what's left — "there are more on the
// server" is as specific as an honest sentence gets.
export const RECORDINGS_LOAD_MORE = (n: number): string => `Load ${n} more`;
export const RECORDINGS_LOADING = "Loading…";
// #510-F9 — pluralise on n: "1 recordings"/"1 recordings are" read wrong for
// the single-item page (the first page off a server with exactly one
// recording, or the last page landing on exactly one more).
export const RECORDINGS_MORE_NOTE = (n: number): string =>
  `${n} recording${n === 1 ? "" : "s"} loaded so far — there are more on the server.`;
export const RECORDINGS_ALL_LOADED = (n: number): string =>
  `All ${n} recording${n === 1 ? "" : "s"} ${n === 1 ? "is" : "are"} loaded.`;
export const RECORDINGS_PAGE_ERROR_TITLE = "Couldn't load more recordings";
export const RECORDINGS_PAGE_ERROR_BODY = (n: number): string =>
  `Wardyn stopped answering partway through. The ${n} already loaded are still here — retry to continue from where it stopped.`;
export const RECORDINGS_FILTER_SCOPE = (n: number): string =>
  `Filters cover the ${n} recordings loaded so far. Load more to search further back.`;

// #459 — visible reasons, not title tooltips. EMPTY_TITLE/EMPTY_BODY is the
// screen's own "nothing has ever run" empty state (runs.length === 0), never
// the "None of your runs have a recording yet" state below it — that one has
// its own e2e pin (ui/e2e/recording.spec.ts) and stays untouched.
export const RECORDINGS = {
  EMPTY_TITLE: "No recordings yet",
  EMPTY_BODY: "Recordings appear once a run's terminal session is captured.",
  SEARCH_DISABLED_HINT:
    "Session recording is disabled on this deployment — there is nothing to search.",
} as const;
