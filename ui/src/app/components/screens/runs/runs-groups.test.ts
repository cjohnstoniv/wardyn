/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { AgentRun } from "../../../lib/types";
import { groupRunsBy } from "./runs-groups";

const run = (over: Partial<AgentRun> = {}): AgentRun => ({
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "Fix flaky auth tests",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://x",
  runner_target: "docker",
  placement: "",
  ...over,
});

describe("groupRunsBy — H-6 Group by Workspace/Title", () => {
  it("groups by workspace, first-appearance order, preserving each group's row order", () => {
    const a = run({ id: "a", repo: "acme/widgets" });
    const b = run({ id: "b", repo: "acme/other" });
    const c = run({ id: "c", repo: "acme/widgets" });
    const groups = groupRunsBy([a, b, c], "workspace");
    expect(groups.map((g) => g.label)).toEqual(["acme/widgets", "acme/other"]);
    expect(groups[0].runs.map((r) => r.id)).toEqual(["a", "c"]);
    expect(groups[1].runs.map((r) => r.id)).toEqual(["b"]);
  });

  it("a run with no repo/workspace_path groups under the NO_REPO label, not a blank one", () => {
    const scratch = run({ id: "s", repo: "", workspace_path: "" });
    const groups = groupRunsBy([scratch], "workspace");
    expect(groups).toEqual([{ label: "Ephemeral scratch — no repo", runs: [scratch] }]);
  });

  it("groups by title (the run's own headline, not the raw task)", () => {
    const a = run({ id: "a", title: "Weekly docs link check" });
    const b = run({ id: "b", title: "Weekly docs link check" });
    const c = run({ id: "c", title: "Something else" });
    const groups = groupRunsBy([a, b, c], "title");
    expect(groups.map((g) => g.label)).toEqual(["Weekly docs link check", "Something else"]);
    expect(groups[0].runs.map((r) => r.id)).toEqual(["a", "b"]);
  });

  it("an empty list groups to nothing", () => {
    expect(groupRunsBy([], "workspace")).toEqual([]);
  });
});
