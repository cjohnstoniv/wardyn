/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { RUN_WAIT } from "../../../../lib/runner-pool-refusals";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import type { AppliedResources } from "../../../../lib/types/new-run-contract";
import { Notices } from "./notices";

afterEach(cleanup);

const narrowedBy = (over: Partial<AppliedResources["cpu_millis"]>): AppliedResources => ({
  cpu_millis: { requested: 16000, applied: 8000, capped: true, ...over },
  memory_mib: { requested: 16384, applied: 8192, capped: true, ...over },
});

describe("Review notices", () => {
  it("names the server's own sentence when the pool is at its limit", () => {
    const full = SCENARIO_PREFLIGHT.full;
    render(<Notices poolName="CI jobs" slots={full.runner_pool_slots} applied={full.resources_applied} askedByYou={false} />);
    expect(screen.getByText(RUN_WAIT.AT_CAPACITY("CI jobs", 40))).toBeInTheDocument();
  });

  it("stays quiet when the run starts now or the pool has no limit", () => {
    const { container, rerender } = render(<Notices poolName="General" slots={{ used: 6, max: 10, waiting: 0, verdict: "starts_now" }} askedByYou={false} />);
    expect(container).toBeEmptyDOMElement();
    rerender(<Notices poolName="General" slots={{ used: 6, waiting: 2, verdict: "waits" }} askedByYou={false} />);
    expect(container).toBeEmptyDOMElement();
  });

  it.each([
    ["the ceiling", { source: "governance", source_name: "Platform team" } as const, NR_REVIEW.NARROWED_BY("Platform team"), NR_REVIEW.NARROWED_TAIL_CEILING],
    ["the pool", { source: "pool", source_name: "CI jobs" } as const, NR_REVIEW.NARROWED_BY("CI jobs"), NR_REVIEW.NARROWED_TAIL_POOL("CI jobs")],
    ["the server", { source: "deployment" } as const, undefined, NR_REVIEW.NARROWED_TAIL_SERVER],
    ["the runners", { source: "runner" } as const, undefined, NR_REVIEW.NARROWED_TAIL_RUNNERS],
  ])("says what narrowed the request: %s", (_name, source, title, tail) => {
    render(<Notices poolName="General" applied={narrowedBy(source)} askedByYou />);
    if (title) expect(screen.getByText(title)).toBeInTheDocument();
    else expect(screen.queryByText(/was narrowed by/)).toBeNull();
    expect(screen.getByText("16 CPU · 16,384 MiB").tagName).toBe("S");
    expect(screen.getByText(`${NR_REVIEW.NARROWED_APPLIES("8 CPU · 8,192 MiB")} ${tail}`)).toBeInTheDocument();
    expect(screen.getByText((_t, el) => el?.tagName === "P" && el.textContent === `16 CPU · 16,384 MiB · ${NR_REVIEW.PROV_ADDED}`)).toBeInTheDocument();
  });

  it("shows the ask as the person's only when they set it, and nothing for a size that was not cut", () => {
    const { container, rerender } = render(<Notices poolName="General" applied={narrowedBy({ source: "governance", source_name: "Platform team" })} askedByYou={false} />);
    expect(screen.queryByText(NR_REVIEW.PROV_ADDED, { exact: false })).toBeNull();
    rerender(<Notices poolName="General" applied={SCENARIO_PREFLIGHT.member.resources_applied} askedByYou />);
    expect(container).toBeEmptyDOMElement();
  });

  it("gives one notice per source when CPU and memory were cut by different limits", () => {
    const applied: AppliedResources = {
      cpu_millis: { requested: 16000, applied: 8000, capped: true, source: "governance", source_name: "Platform team" },
      memory_mib: { requested: 16384, applied: 8192, capped: true, source: "pool", source_name: "General" },
    };
    render(<Notices poolName="General" applied={applied} askedByYou />);
    expect(screen.getByText(NR_REVIEW.NARROWED_BY("Platform team"))).toBeInTheDocument();
    expect(screen.getByText(NR_REVIEW.NARROWED_BY("General"))).toBeInTheDocument();
  });

  it("lists the server's warnings as it wrote them, and drops the resources-capped line when the narrowed notice says it", () => {
    const warn = ["inline_policy: allowed_domains narrowed by your ceiling", "resources capped to operator maximum"];
    render(<Notices poolName="General" applied={narrowedBy({ source: "governance", source_name: "Platform team" })} askedByYou={false} warnings={warn} />);
    expect(screen.getByText(NR_REVIEW.WARNINGS_TITLE)).toBeInTheDocument();
    expect(screen.getByText(warn[0])).toBeInTheDocument();
    expect(screen.getByText(NR_REVIEW.NARROWED_BY("Platform team"))).toBeInTheDocument();
    expect(screen.queryByText(warn[1])).toBeNull();
  });

  it("shows the resources-capped line when no CPU or memory cut is rendered (a pids or disk cut)", () => {
    render(<Notices askedByYou={false} warnings={["resources capped to operator maximum"]} />);
    expect(screen.getByText("resources capped to operator maximum")).toBeInTheDocument();
  });

  it("keeps two identical warnings from two sources", () => {
    render(<Notices askedByYou={false} warnings={["same", "same"]} />);
    expect(screen.getAllByText("same")).toHaveLength(2);
  });

  it("shows no warnings notice when there are none", () => {
    const { container } = render(<Notices askedByYou={false} warnings={[]} />);
    expect(container).toBeEmptyDOMElement();
  });
});
