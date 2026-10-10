/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { AgentRun, RunState } from "../../../lib/types";
import { INTERACTIVE_HEADLINE, NO_REPO, repoLabel, rowHeadline } from "./board-groups";

const run = (over: Partial<AgentRun> = {}): AgentRun => ({
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "Fix flaky auth tests",
  confinement_class: "CC2",
  state: "RUNNING" as RunState,
  spiffe_id: "spiffe://x",
  runner_target: "docker",
  placement: "",
  ...over,
});

describe("rowHeadline — the honest fallback chain", () => {
  it("prefers the title", () => {
    expect(rowHeadline(run({ title: "My title", task: "task text" }))).toBe("My title");
  });

  it("falls back to the task when there is no title", () => {
    expect(rowHeadline(run({ title: "", task: "Fix the thing" }))).toBe("Fix the thing");
  });

  it("falls back to the workspace path's basename when there is no title or task", () => {
    expect(rowHeadline(run({ title: "", task: "", workspace_path: "/home/op/acme-widgets/" }))).toBe(
      "acme-widgets",
    );
  });

  it("claims 'Interactive session' only for a run that IS one", () => {
    expect(rowHeadline(run({ title: "", task: "", workspace_path: "", interactive: true }))).toBe(
      INTERACTIVE_HEADLINE,
    );
  });

  it("a nameless non-interactive run gets the dash, never the interactive claim", () => {
    expect(rowHeadline(run({ title: "", task: "", workspace_path: "", interactive: false }))).toBe("—");
  });
});

describe("repoLabel", () => {
  it("prefers repo, mono", () => {
    expect(repoLabel(run({ repo: "acme/widgets" }))).toEqual({ text: "acme/widgets", mono: true });
  });

  it("falls back to workspace_path", () => {
    expect(repoLabel(run({ repo: "", workspace_path: "/scratch/x" }))).toEqual({
      text: "/scratch/x",
      mono: true,
    });
  });

  it("neither present reads as the honest no-repo phrase, not mono", () => {
    expect(repoLabel(run({ repo: "", workspace_path: "" }))).toEqual({ text: NO_REPO, mono: false });
  });
});
