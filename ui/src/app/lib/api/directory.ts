/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Directory autocomplete (0.7 §I / PF-29) — the client half of
// GET /access/directory/search. Mirrors internal/api/directory_search.go and
// internal/directory's Entry contract: DisplayName is what a UI RENDERS,
// ClaimValue is what it STORES, and for a group those are deliberately
// different strings (the `groups` claim carries an object GUID, never a name).
//
// The one thing this module decides is ABSENT vs BROKEN. The endpoint answers a
// distinct 503 code when no connector is configured, and that is not an error:
// the deployment simply never turned the feature on, which is the common case.
// search() returns null for that — the caller degrades to a plain text input
// with no error surface — and LATCHES it, so a deployment with no directory
// costs one request per session rather than one per keystroke. Every other
// failure throws, because "your directory is broken" is a thing an admin has to
// be told.
import { asJson, unwrapList, wfetch } from "./core";

// The three classes of directory object an entry can come from
// (directory.KindUser / KindGroup / KindAppRole).
export type DirectoryKind = "user" | "group" | "approle";

// What a search asks for. "any" is IN the v1 contract (directory.KindAny), not
// a convenience: the People step's Value field is one kind-LESS input that
// accepts an App Role, a group or an email.
export type DirectorySearchKind = DirectoryKind | "any";

// directory.Entry on the wire.
export interface DirectoryEntry {
  display_name: string;
  claim_value: string;
  kind: DirectoryKind;
  /** Presentation-only disambiguator — two people share a display name. */
  detail?: string;
}

// directory.MinQueryLen, mirrored ONCE. The endpoint 400s a shorter q, so a
// caller gates on this constant rather than sending a request it knows will be
// refused — and rather than repeating the literal 2 beside every field.
export const MIN_QUERY_LEN = 2;

// directory_search.go's directoryUnconfiguredCode.
const UNCONFIGURED_CODE = "directory_unconfigured";

// Session latch: a server that reports no provider will not grow one while this
// tab is open, so stop asking.
let unconfigured = false;

export const directory = {
  /**
   * Search the configured directory. Resolves to null — never throws — when no
   * provider is configured; that is the console's absent-mode signal and the
   * caller must render nothing at all for it. Throws HttpError on a real
   * failure.
   */
  async search(q: string, kind: DirectorySearchKind = "any"): Promise<DirectoryEntry[] | null> {
    if (unconfigured) return null;
    const res = await wfetch(`/access/directory/search?q=${encodeURIComponent(q)}&type=${kind}`, { method: "GET" });
    if (res.status === 503) {
      // clone(): asJson below reads the body too, and a Response body is read
      // exactly once (access.ts's parseJsonBody idiom). A 503 WITHOUT the code
      // — a proxy's, say — falls through and throws: that one really is broken.
      const body = (await res
        .clone()
        .json()
        .catch(() => null)) as { code?: unknown } | null;
      if (body?.code === UNCONFIGURED_CODE) {
        unconfigured = true;
        return null;
      }
    }
    const payload = await asJson<{ results?: DirectoryEntry[] }>(res);
    return unwrapList<DirectoryEntry>(payload.results);
  },
};
