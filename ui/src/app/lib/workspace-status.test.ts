/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// This suite used to pin a three-word pipeline vocabulary — "Setting up" while
// a scan was pending or running, "Usable" once scanned, "Scan failed" on error.
// The 0.5 dialog does one POST and no scan, so nothing ever left "Setting up":
// a new workspace wore an amber progress chip forever, for work that was never
// going to happen. What is pinned now is that the console cannot say that again.
import { describe, it, expect } from "vitest";
import { statusWord, statusTone, storySentence, isUsable } from "./workspace-status";
import type { Workspace, WorkspaceStatus } from "./types";

const ALL: WorkspaceStatus[] = ["pending_scan", "scanning", "scanned", "error"];

const ws = (over: Partial<Workspace> = {}): Workspace => ({
  id: "w",
  name: "n",
  kind: "repo",
  source: "s",
  status: "scanned",
  created_at: "",
  updated_at: "",
  ...over,
});

describe("statusWord", () => {
  it("a freshly created workspace reads Ready, not a progress state", () => {
    expect(statusWord("scanned")).toBe("Ready");
  });

  // The regression that produced the permanent amber chip. Rows written before
  // 0.5 still carry these; migration 0036 heals them, and this is the belt.
  it("legacy pending_scan/scanning rows read Ready — nothing was going to scan them", () => {
    expect(statusWord("pending_scan")).toBe("Ready");
    expect(statusWord("scanning")).toBe("Ready");
  });

  it("error names the import that failed, not a scan that no longer exists", () => {
    expect(statusWord("error")).toBe("Import failed");
  });

  it("an unrecognized status reads Ready — attachment never depended on this field", () => {
    expect(statusWord("something_future" as WorkspaceStatus)).toBe("Ready");
  });

  it("never says 'Setting up' for ANY status — the state it described is gone", () => {
    for (const s of [...ALL, "something_future" as WorkspaceStatus]) {
      expect(statusWord(s)).not.toBe("Setting up");
    }
  });
});

describe("statusTone", () => {
  it("is success for Ready and danger for Import failed", () => {
    expect(statusTone("scanned")).toEqual({ tone: "success" });
    expect(statusTone("error")).toEqual({ tone: "danger" });
  });

  // The amber/pulsing treatments existed only to animate scan progress.
  it("no status renders as in-progress — nothing pulses and nothing is amber", () => {
    for (const s of ALL) {
      const { tone, pulse } = statusTone(s);
      expect(pulse).toBeUndefined();
      expect(tone).not.toBe("warning");
      expect(tone).not.toBe("info");
    }
  });

  it("never disagrees with statusWord's bucket for any status", () => {
    for (const s of ALL) {
      expect(statusTone(s).tone).toBe(statusWord(s) === "Import failed" ? "danger" : "success");
    }
  });
});

describe("storySentence", () => {
  it("answers 'can I use this', not 'how far along is a pipeline'", () => {
    expect(storySentence(ws({ status: "scanned" }))).toBe("Runs can attach this now.");
    expect(storySentence(ws({ status: "pending_scan" }))).toBe("Runs can attach this now.");
    expect(storySentence(ws({ status: "scanning" }))).toBe("Runs can attach this now.");
  });

  it("an errored workspace says what failed AND that runs can still attach it", () => {
    const s = storySentence(ws({ status: "error" }));
    expect(s).toMatch(/import run failed/i);
    expect(s).toMatch(/can still attach/i);
  });

  it("never promises a scan", () => {
    for (const st of ALL) {
      expect(storySentence(ws({ status: st }))).not.toMatch(/scan/i);
    }
  });
});

describe("isUsable", () => {
  // An import failure never blocked attachment; gating on it would hide a
  // perfectly usable workspace behind a red chip.
  it("is true for every status, including error", () => {
    for (const s of ALL) expect(isUsable(s)).toBe(true);
  });
});
