/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { statusWord, statusTone, storySentence } from "./workspace-status";
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

describe("statusWord", () => {
  it("maps the four wire statuses", () => {
    expect(statusWord("pending_scan")).toBe("Setting up");
    expect(statusWord("scanning")).toBe("Setting up");
    expect(statusWord("scanned")).toBe("Usable");
    expect(statusWord("error")).toBe("Scan failed");
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

  it("never disagrees with statusWord's bucket for any status", () => {
    const all: WorkspaceStatus[] = ["pending_scan", "scanning", "scanned", "error"];
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
});
