/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { statusWord, statusTone, storySentence } from "./workspace-status";
import { C } from "./workspace-copy";
import type { Workspace, WorkspaceStatus } from "./types";

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

// Every legacy status statusWord/statusTone/storySentence must tolerate until
// the migration wave drops them from the wire (import-types.ts's
// activeStepForStatus resumes the same set onto Record for the same reason).
const LEGACY_USABLE: WorkspaceStatus[] = ["ready", "verifying", "verify_failed", "building", "build_error"];

describe("statusWord", () => {
  it("maps the three current statuses", () => {
    expect(statusWord("pending_scan")).toBe("Setting up");
    expect(statusWord("scanning")).toBe("Setting up");
    expect(statusWord("scanned")).toBe("Usable");
    expect(statusWord("error")).toBe("Scan failed");
  });

  it("is legacy-tolerant: every retired build/verify status reads as Usable", () => {
    for (const status of LEGACY_USABLE) {
      expect(statusWord(status)).toBe("Usable");
    }
  });

  it("defaults an unrecognized status to Setting up, not a crash or 'Usable'", () => {
    expect(statusWord("something_future" as WorkspaceStatus)).toBe("Setting up");
  });
});

describe("statusTone", () => {
  it("splits Setting up into a plain warning vs. a pulsing info dot while actively scanning", () => {
    expect(statusTone("pending_scan")).toEqual({ tone: "warning" });
    expect(statusTone("scanning")).toEqual({ tone: "info", pulse: true });
  });

  it("is success for Usable and danger for Scan failed", () => {
    expect(statusTone("scanned")).toEqual({ tone: "success" });
    expect(statusTone("error")).toEqual({ tone: "danger" });
  });

  it("agrees with statusWord for every legacy Usable status", () => {
    for (const status of LEGACY_USABLE) {
      expect(statusTone(status)).toEqual({ tone: "success" });
    }
  });

  it("never disagrees with statusWord's bucket for any status", () => {
    const all: WorkspaceStatus[] = [
      "pending_scan",
      "scanning",
      "scanned",
      "building",
      "build_error",
      "verifying",
      "verify_failed",
      "ready",
      "error",
    ];
    for (const status of all) {
      const word = statusWord(status);
      const tone = statusTone(status).tone;
      if (word === "Usable") expect(tone).toBe("success");
      if (word === "Scan failed") expect(tone).toBe("danger");
      if (word === "Setting up") expect(["warning", "info"]).toContain(tone);
    }
  });
});

describe("storySentence", () => {
  it("pins the mock's storyFor sentences verbatim, em-dashes included", () => {
    expect(storySentence(ws({ status: "scanning" }))).toBe("Scanning the source now.");
    expect(storySentence(ws({ status: "error" }))).toBe(
      "The last scan failed, so there's no profile — a run gets no detected egress, secrets or services.",
    );
    expect(storySentence(ws({ status: "pending_scan" }))).toBe(
      "Not scanned yet — runs can attach it, nothing is attached automatically.",
    );
    expect(storySentence(ws({ status: "scanned" }))).toBe("Runs can attach this now.");
  });

  it("appends C.IMAGE_ENV for a container-kind workspace, exactly like the mock", () => {
    expect(storySentence(ws({ status: "scanned", kind: "container" }))).toBe(`Runs can attach this now. ${C.IMAGE_ENV}`);
  });

  it("every legacy Usable status reads the same story as a plain scanned workspace", () => {
    for (const status of LEGACY_USABLE) {
      expect(storySentence(ws({ status }))).toBe("Runs can attach this now.");
    }
  });
});
