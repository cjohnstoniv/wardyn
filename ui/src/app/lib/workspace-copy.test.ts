/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { C } from "./workspace-copy";

// Sentinel byte-exact pins against the approved mock export
// (mockup/wardyn-workspaces.js `C`) — a hand-retyped copy could silently
// drift (an em-dash swapped for a hyphen, a dropped "not"); these lock a
// sample verbatim, em-dashes included.
describe("workspace-copy — sentinel byte-exact pins", () => {
  it("pins C entries verbatim, em-dashes included", () => {
    expect(C.DECLARED).toBe("Declared by workspace files (untrusted) — names only, values are never read.");
    expect(C.REPO_RO).toBe(
      "Repos are cloned fresh into the sandbox — nothing on your machine is touched, so there's nothing to protect with read-only.",
    );
  });
});

// W9-S1-5: BLIND_SPOT must match the scanner's REAL bounds
// (internal/workspacescan/scan.go's maxDepth=6, maxManifestHits, maxFileBytes;
// detect.go's maxDetectLines) rather than a stale "4 levels" that never
// tracked a depth the scanner raised to 6, and must name the per-scan file
// budget / per-file size-or-line caps it was silently omitting entirely.
describe("workspace-copy — BLIND_SPOT matches the scanner's real bounds", () => {
  it("states the real walk depth (6, not the stale 4) and the file/size caps it used to omit", () => {
    expect(C.BLIND_SPOT).toContain("6 levels");
    expect(C.BLIND_SPOT).not.toContain("4 levels");
    expect(C.BLIND_SPOT.toLowerCase()).toMatch(/file budget|manifest/);
    expect(C.BLIND_SPOT).toMatch(/1 ?MB|1 ?MiB/i);
  });
});

// Every string here is user-facing copy — "harness" is internal jargon (see
// AI_TYPES/CAPS in integrations.ts) that the approved copy deliberately never
// says out loud (it says "agent tool"/"AI integration" instead). This catches
// a future string regressing back to the internal term.
describe("workspace-copy — no exported string leaks the word 'harness'", () => {
  it("checks every C value", () => {
    const allStrings = Object.values(C).filter((v): v is string => typeof v === "string");
    expect(allStrings.length).toBeGreaterThan(5); // sanity: didn't accidentally test an empty set
    for (const s of allStrings) {
      expect(s.toLowerCase()).not.toContain("harness");
    }
  });
});
