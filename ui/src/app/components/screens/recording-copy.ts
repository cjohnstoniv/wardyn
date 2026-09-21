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
export const RECORDINGS_MORE_NOTE = (n: number): string =>
  `${n} recordings loaded so far — there are more on the server.`;
export const RECORDINGS_ALL_LOADED = (n: number): string => `All ${n} recordings are loaded.`;
export const RECORDINGS_PAGE_ERROR_TITLE = "Couldn't load more recordings";
export const RECORDINGS_PAGE_ERROR_BODY = (n: number): string =>
  `Wardyn stopped answering partway through. The ${n} already loaded are still here — retry to continue from where it stopped.`;
export const RECORDINGS_FILTER_SCOPE = (n: number): string =>
  `Filters cover the ${n} recordings loaded so far. Load more to search further back.`;
