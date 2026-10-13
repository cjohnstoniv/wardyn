/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pool area's notes about a person's own user pools (mock M-POOLS v6 C1 to C5). Strings are written out.
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunnerPoolCatalogueEntry } from "../../../../../lib/types/runner-pools";
import { PoolNotes } from "./pool-notes";

const G = 1024;
const tally = (runs: number, c: number, m: number, d: number) => ({ runs, cpu_millis: c * 1000, memory_mib: m * G, disk_mib: d * G });
const pool = (over: Partial<RunnerPoolCatalogueEntry> = {}): RunnerPoolCatalogueEntry => ({
  id: "p1", name: "Laptop", scope: "user", availability: "available", run_types: ["background", "interactive"], barrier_options: [], ...over,
});
const capacity = (availableRuns: number) => ({
  total: tally(3, 6, 12, 200), in_use: tally(3 - availableRuns, 4, 8, 100), available: tally(availableRuns, 2, 4, 100),
  largest_per_run: { cpu_millis: 4000, memory_mib: 16 * G, disk_mib: 100 * G }, runners: 2, runners_online: 2,
});
const show = (pools: RunnerPoolCatalogueEntry[], chosen?: RunnerPoolCatalogueEntry) =>
  render(
    <MemoryRouter>
      <PoolNotes pools={pools} chosen={chosen} runType="background" />
    </MemoryRouter>,
  );

describe("New Run pool notes", () => {
  it("with no user pool, says so and links to creating one", () => {
    show([pool({ id: "g", name: "General", scope: "organization" })]);
    expect(screen.getByText("No user pools yet.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Create a pool" })).toHaveAttribute("href", "/account/pools/new");
  });

  it("with pools none of the person's runners are in, links to My runner", () => {
    show([pool({ availability: "unavailable", reason: "runner_pool_no_eligible_member" })]);
    expect(screen.getByText("None of your runners are in a pool yet.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Add your runner to a pool" })).toHaveAttribute("href", "/account#my-runner");
  });

  it("names the pool whose runner is offline", () => {
    show([pool({ availability: "unavailable", reason: "runner_pool_no_online_runner", my_runners: [{ id: "r1", name: "laptop-7", online: false }] })]);
    expect(screen.getByText("Your runner in Laptop is offline.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Check your runner" })).toBeInTheDocument();
  });

  it("with a user pool chosen, says the biggest run it can take, and that a new run waits when every runner is at its limit", () => {
    const { unmount } = show([], pool({ capacity: capacity(1) }));
    expect(screen.getByText("Biggest run it can take: 4 CPUs · 16 GB memory · 100 GB disk")).toBeInTheDocument();
    expect(screen.queryByText(/A new run waits/)).toBeNull();
    unmount();
    show([], pool({ capacity: capacity(0) }));
    expect(screen.getByRole("status")).toHaveTextContent("A new run waits. Every runner of yours in Laptop is at its limit. It starts when one has room.");
    expect(screen.getByRole("link", { name: "See your runners" })).toBeInTheDocument();
  });

  it("says nothing about an organization pool, or when a user pool is usable and none is chosen", () => {
    const { container, unmount } = show([], pool({ scope: "organization", capacity: capacity(0) }));
    expect(container).toBeEmptyDOMElement();
    unmount();
    expect(show([pool()]).container).toBeEmptyDOMElement();
  });
});
