/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { C, V2C, RD } from "./workspace-copy";

// Sentinel byte-exact pins against the approved mock export
// (mockup/wardyn-workspaces.js `C`/`V2C`, mockup/wardyn-rundeltas.js `RD`) — a
// hand-retyped copy could silently drift (an em-dash swapped for a hyphen, a
// dropped "not"); these lock a sample verbatim, em-dashes included.
describe("workspace-copy — sentinel byte-exact pins", () => {
  it("pins C entries verbatim, em-dashes included", () => {
    expect(C.DECLARED).toBe("Declared by workspace files (untrusted) — names only, values are never read.");
    expect(C.IMAGE_ENV).toBe("Container images aren't scanned — the image is the environment.");
    expect(C.REPO_RO).toBe(
      "Repos are cloned fresh into the sandbox — nothing on your machine is touched, so there's nothing to protect with read-only.",
    );
  });

  it("pins V2C entries verbatim", () => {
    expect(V2C.FLOOR).toBe("Every workspace has at least one source; this scratch directory is the floor.");
    // The tools-not-AI round: the step-② blurb ends at "fit" (no agent-tool
    // clause), the no-inject law carries no Claude clause, and the two
    // integration-coupling sentences (HARNESS_ON/OFF) are DELETED — pinned
    // absent so they can't quietly return.
    expect(V2C.S2_BLURB.endsWith("suggests images that fit.")).toBe(true);
    expect(V2C.IMG_NO_INJECT).toBe("Wardyn doesn't inspect the image and never injects tools into it.");
    expect(V2C).not.toHaveProperty("HARNESS_ON");
    expect(V2C).not.toHaveProperty("HARNESS_OFF");
  });

  it("pins RD entries verbatim", () => {
    expect(RD.NONE_LINE).toBe("No integration can drive Claude Code. This run launches; its first model call fails.");
    expect(RD.EXEC_LINE).toBe("Governed command — no model access is wired, and nothing suggests otherwise.");
  });
});

// Every string here is user-facing copy — "harness" is internal jargon (see
// AI_TYPES/CAPS in integrations.ts, and V2C's own HARNESS_ON/HARNESS_OFF KEYS)
// that the approved copy deliberately never says out loud (it says "agent
// tool"/"AI integration" instead). This catches a future string regressing
// back to the internal term.
describe("workspace-copy — no exported string leaks the word 'harness'", () => {
  it("checks every C, V2C, and RD value", () => {
    const allStrings = [...Object.values(C), ...Object.values(V2C), ...Object.values(RD)];
    expect(allStrings.length).toBeGreaterThan(30); // sanity: didn't accidentally test an empty set
    for (const s of allStrings) {
      expect(s.toLowerCase()).not.toContain("harness");
    }
  });
});
