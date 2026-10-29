/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { AgentRun } from "../../../lib/types";
import { filterBySegment, runSegment, segmentCounts } from "./runs-segment";

const run = (over: Partial<AgentRun> = {}): AgentRun => ({ id: "r", agent: "", ...over }) as AgentRun;
const service: AgentRun["service"] = { restart: "on_failure" };

describe("runSegment", () => {
  it.each([
    ["agent_task", "agent_tasks"],
    ["environment", "environments"],
    ["application", "apps"],
  ] as const)("a stored %s shape is %s", (shape, seg) => {
    expect(runSegment(run({ shape }))).toBe(seg);
  });

  it("a stored shape wins over the fallback signals", () => {
    expect(runSegment(run({ shape: "application", agent: "claude-code", experience: "interactive" }))).toBe("apps");
  });

  it.each(["agent_task", "environment", "application"] as const)("a service is a Service whatever its shape (%s)", (shape) => {
    expect(runSegment(run({ shape, service }))).toBe("services");
  });

  it("no shape, a service: Services", () => {
    expect(runSegment(run({ service }))).toBe("services");
  });

  it("no shape, a harness: Agent tasks, interactive or not", () => {
    expect(runSegment(run({ agent: "claude-code", experience: "background" }))).toBe("agent_tasks");
    expect(runSegment(run({ agent: "claude-code", experience: "interactive" }))).toBe("agent_tasks");
  });

  it("no shape, no harness, interactive: Environments (experience or the legacy flag)", () => {
    expect(runSegment(run({ experience: "interactive" }))).toBe("environments");
    expect(runSegment(run({ interactive: true }))).toBe("environments");
  });

  it("no shape, no harness, not interactive: Apps & scripts", () => {
    expect(runSegment(run({ experience: "background" }))).toBe("apps");
    expect(runSegment(run())).toBe("apps");
  });
});

describe("segmentCounts and filterBySegment", () => {
  const runs = [
    run({ id: "a", shape: "agent_task" }),
    run({ id: "e", shape: "environment" }),
    run({ id: "p", shape: "application" }),
    run({ id: "s", shape: "application", service }),
    run({ id: "n" }),
  ];

  it("the four segments sum to All", () => {
    const c = segmentCounts(runs);
    expect(c).toEqual({ all: 5, agent_tasks: 1, environments: 1, apps: 2, services: 1 });
    expect(c.agent_tasks + c.environments + c.apps + c.services).toBe(c.all);
  });

  it("filters to one segment, and All keeps every run", () => {
    expect(filterBySegment(runs, "apps").map((r) => r.id)).toEqual(["p", "n"]);
    expect(filterBySegment(runs, "all")).toBe(runs);
  });
});
