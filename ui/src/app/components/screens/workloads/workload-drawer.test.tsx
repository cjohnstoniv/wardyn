/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Workload } from "../../../lib/types/workloads";
import { WORKLOADS as W } from "../../wardyn/copy/workloads";
import { WorkloadDrawer } from "./workload-drawer";

const getRun = vi.fn();
vi.mock("../../../lib/api/runs", () => ({ runs: { getRun: (id: string) => getRun(id) } }));
const health = vi.fn();
vi.mock("../../../lib/api/health", () => ({ health: { health: () => health() } }));
const history = vi.fn();
vi.mock("../../../lib/api/workloads", () => ({
  workloads: { restart: vi.fn(), remove: vi.fn(), history: (name: string, owner?: string) => history(name, owner) },
}));

const RUN = "7c41e0a9-0000-0000-0000-000000000000";
const DIGEST = "9f1c4e5d2b7a3c8e0f6a1b9d4c7e2a5b8f3d6c9e1a4b7d0f3c6e9a2b5d8a07b";
const NOW = Date.now();
const D = 24 * 60 * 60 * 1000;
const ago = (ms: number) => new Date(NOW - ms).toISOString();

const workload = (over: Partial<Workload> = {}): Workload => ({
  name: "billing-api",
  owner: "me",
  source: "api",
  intent: {},
  generation: 4,
  current_generation: 4,
  run_id: RUN,
  state: "RUNNING",
  created_at: ago(9 * D),
  updated_at: ago(2 * D),
  ...over,
});

function open(w: Workload) {
  return render(
    <MemoryRouter>
      <WorkloadDrawer workload={w} view="user" now={NOW} onClose={vi.fn()} onChanged={vi.fn()} />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getRun.mockResolvedValue({ id: RUN, created_at: ago(2 * D) });
  history.mockResolvedValue([]);
  health.mockResolvedValue({ status: "ok", ui_sandbox: { inbound_url_template: "https://{run}.apps.example.com/r/{run}/{app}/" } });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("WorkloadDrawer digest", () => {
  it("shows the digest short, with the full value on hover", async () => {
    open(workload({ digest: DIGEST }));
    await screen.findByText(/started/);
    expect(screen.getByText(W.DIGEST)).toBeInTheDocument();
    const short = screen.getByText("sha256:9f1c4e…a07b");
    expect(short).toHaveAttribute("title", DIGEST);
  });

  it("draws no Digest row for a reader the server withheld it from", async () => {
    open(workload());
    await screen.findByText(/started/);
    expect(screen.queryByText(W.DIGEST)).toBeNull();
  });
});

describe("WorkloadDrawer history", () => {
  const OLD = "51c0d7e2-0000-0000-0000-000000000000";

  it("lists launches and refusals as the board does, newest first, with the run a launch replaced", async () => {
    history.mockResolvedValue([
      { time: ago(2 * D), action: "replace", generation: 4, run_id: RUN, ended_run_id: OLD },
      { time: ago(2 * D), action: "launch", generation: 4, run_id: RUN },
      { time: ago(2 * D), action: "apply", generation: 4 },
      { time: ago(3 * D), action: "launch_refused", generation: 3, reason: "pool_lifetime_exceeded" },
      { time: ago(9 * D), action: "launch", generation: 2, run_id: OLD },
    ]);
    open(workload({ owner: "service:billing" }));
    const list = await screen.findByTestId("workload-history");
    expect(history).toHaveBeenCalledWith("billing-api", "service:billing");
    expect(screen.getByText(W.HISTORY)).toBeInTheDocument();
    expect(within(list).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "gen 4 launched 7c41e0a9, ended 51c0d7e2 · 2 d ago",
      "gen 3 refused · 3 d ago",
      "gen 2 launched 51c0d7e2 · 9 d ago",
    ]);
  });

  it("draws no History when there are no rows or the read fails", async () => {
    history.mockRejectedValue(new Error("no"));
    open(workload());
    await screen.findByText(/started/);
    expect(screen.queryByText(W.HISTORY)).toBeNull();
  });
});

describe("WorkloadDrawer inbound address", () => {
  it("shows the current run's address, built from /healthz's template as the run page builds it", async () => {
    getRun.mockResolvedValue({
      id: RUN,
      created_at: ago(2 * D),
      ui_apps: [{ name: "api", port: 8080, inbound: true }, { name: "admin", port: 9090 }],
    });
    open(workload());
    expect(await screen.findByText(`https://${RUN}.apps.example.com/r/${RUN}/api/`)).toBeInTheDocument();
    expect(screen.getByText(W.INBOUND_ADDRESS)).toBeInTheDocument();
    expect(screen.queryByText(/\/admin\//)).toBeNull();
  });

  it("draws no address for a run with no inbound app, and does not ask /healthz", async () => {
    getRun.mockResolvedValue({ id: RUN, created_at: ago(2 * D), ui_apps: [{ name: "admin", port: 9090 }] });
    open(workload());
    await screen.findByText(/started/);
    expect(screen.queryByText(W.INBOUND_ADDRESS)).toBeNull();
    expect(health).not.toHaveBeenCalled();
  });

  it("draws no address when the deployment publishes no template", async () => {
    getRun.mockResolvedValue({ id: RUN, created_at: ago(2 * D), ui_apps: [{ name: "api", port: 8080, inbound: true }] });
    health.mockResolvedValue({});
    open(workload());
    await screen.findByText(/started/);
    await waitFor(() => expect(health).toHaveBeenCalled());
    expect(screen.queryByText(W.INBOUND_ADDRESS)).toBeNull();
  });
});
