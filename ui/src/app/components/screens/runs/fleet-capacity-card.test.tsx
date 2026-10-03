/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.8.6 fleet-fl4 (M6) — the admin Fleet capacity card over
// GET /admin/runs/capacity: the "configured reservations" label, the unknown
// count, a basis per runner, owner truncation, and the security-operator gate.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { OperatorProvider } from "../../wardyn/operator-context";
import { FleetCapacityCard } from "./fleet-capacity-card";
import type { RunCapacityResponse, RunCapacitySums } from "../../../lib/types";

const getMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: { getAdminRunCapacity: (...a: unknown[]) => getMock(...a) },
}));

const sums = (o: Partial<RunCapacitySums> = {}): RunCapacitySums => ({
  holding: 0,
  unknown: 0,
  agent_cpu_request_millis: 0,
  agent_cpu_limit_millis: 0,
  agent_memory_request_mib: 0,
  agent_memory_limit_mib: 0,
  proxy_cpu_millis: 0,
  proxy_memory_mib: 0,
  proxy_cpu_uncapped: 0,
  held_cpu_millis: 0,
  held_memory_mib: 0,
  ...o,
});

const k8s = sums({
  holding: 14,
  unknown: 3,
  agent_cpu_request_millis: 30000,
  agent_cpu_limit_millis: 30000,
  agent_memory_request_mib: 53248,
  agent_memory_limit_mib: 53248,
  proxy_cpu_millis: 6000,
  proxy_memory_mib: 9216,
  held_cpu_millis: 36000,
  held_memory_mib: 62464,
});

function response(o: Partial<RunCapacityResponse> = {}): RunCapacityResponse {
  return {
    generated_at: "2026-10-03T12:00:00Z",
    basis: "configured_reservations",
    states: { RUNNING: 9, STARTING: 2, WAITING_FOR_CONFIRMATION: 1, PENDING: 3 },
    paused: 2,
    kept: 1,
    totals: { basis: "requests", ...k8s },
    age_buckets: [
      { bucket: "under_1h", count: 4 },
      { bucket: "1h_to_8h", count: 6 },
      { bucket: "8h_to_24h", count: 2 },
      { bucket: "1d_to_7d", count: 2 },
      { bucket: "over_7d", count: 0 },
    ],
    by_runner: { k8s: { basis: "requests", ...k8s } },
    by_owner: [],
    by_owner_truncated: false,
    unschedulable: [],
    unschedulable_total: 0,
    ...o,
  };
}

function renderCard(securityOperator = true) {
  return render(
    <MemoryRouter initialEntries={["/admin/runs"]}>
      <OperatorProvider operator securityOperator={securityOperator}>
        <FleetCapacityCard />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

async function expand() {
  await userEvent.click(await screen.findByRole("button", { name: /Fleet capacity/ }));
}

beforeEach(() => vi.clearAllMocks());

describe("FleetCapacityCard", () => {
  it("renders nothing, and never reads, for a non-security-operator", async () => {
    getMock.mockResolvedValue(response());
    const { container } = renderCard(false);
    await new Promise((r) => setTimeout(r, 0));
    expect(container).toBeEmptyDOMElement();
    expect(getMock).not.toHaveBeenCalled();
  });

  it("collapsed summary says configured reservations and the unknown count", async () => {
    getMock.mockResolvedValue(response());
    renderCard();
    const card = await screen.findByTestId("fleet-capacity-card");
    await waitFor(() =>
      expect(card).toHaveTextContent("14 runs · 36 CPU · 61 GiB configured reservations · 3 unknown"),
    );
    expect(card.textContent).not.toMatch(/\b(used|allocated)\b/i);
    expect(screen.queryByText(/Waiting for room/)).toBeNull();
  });

  it("shows the Waiting for room chip only when runs are waiting, and links each run", async () => {
    getMock.mockResolvedValue(
      response({
        unschedulable_total: 2,
        unschedulable: [
          {
            id: "3f9c1a2b-0000-4000-8000-000000000001",
            owner: "ana@example.com",
            waited_seconds: 7200,
            reason: "Unschedulable",
            runner_kind: "k8s",
            agent_cpu_request_millis: 2000,
            agent_memory_request_mib: 4096,
            proxy_cpu_millis: 500,
            proxy_memory_mib: 256,
          },
        ],
      }),
    );
    renderCard();
    const card = await screen.findByTestId("fleet-capacity-card");
    await waitFor(() => expect(card).toHaveTextContent("Waiting for room · 2"));
    await expand();
    const waiting = screen.getByTestId("fleet-capacity-waiting");
    const link = within(waiting).getByRole("link", { name: "3f9c1a2b" });
    expect(link).toHaveAttribute("href", "/admin/runs/3f9c1a2b-0000-4000-8000-000000000001");
    expect(waiting).toHaveTextContent("Waiting for a machine with room for this sandbox.");
    expect(waiting).toHaveTextContent("since 2h ago");
    expect(waiting).toHaveTextContent("2.5 CPU · 4.3 GiB");
    expect(waiting).toHaveTextContent("Showing the oldest 1 of 2.");
  });

  it("Kubernetes block carries its basis, the unknown line and its hint", async () => {
    getMock.mockResolvedValue(response());
    renderCard();
    await expand();
    const block = screen.getByTestId("fleet-capacity-runner-k8s");
    expect(block).toHaveTextContent("Kubernetes · requests");
    expect(block).toHaveTextContent("What the scheduler sets aside and your namespace quota charges.");
    expect(block).toHaveTextContent("Agent requests 30 CPU · 52 GiB");
    expect(block).toHaveTextContent("Agent limits 30 CPU · 52 GiB");
    expect(block).toHaveTextContent("Proxy 6 CPU · 9 GiB");
    expect(block).toHaveTextContent("3 unknown — not in these totals.");
    expect(block).toHaveTextContent("Runs started before 0.8.6 recorded no reservation");
    expect(screen.getByTestId("fleet-capacity-states")).toHaveTextContent(
      "Running 9 · Starting 2 · Awaiting confirmation 1 · Paused 2 · Pending 3 · Kept 1",
    );
    expect(screen.getByTestId("fleet-capacity-age")).toHaveTextContent("Under 1h 4 · 1–8h 6 · 8–24h 2 · 1–7d 2 · Over 7d 0");
  });

  it("Docker block is caps, and an uncapped proxy CPU reads no cap", async () => {
    const docker = sums({
      holding: 2,
      agent_cpu_limit_millis: 4000,
      agent_memory_limit_mib: 8192,
      proxy_memory_mib: 512,
      proxy_cpu_uncapped: 2,
      held_cpu_millis: 0,
      held_memory_mib: 512,
    });
    getMock.mockResolvedValue(
      response({
        totals: { basis: "caps", ...docker },
        by_runner: { docker: { basis: "caps", ...docker } },
      }),
    );
    renderCard();
    await expand();
    const block = screen.getByTestId("fleet-capacity-runner-docker");
    expect(block).toHaveTextContent("Docker · caps");
    expect(block).toHaveTextContent("Docker sets nothing aside.");
    expect(block).toHaveTextContent("Agent caps 4 CPU · 8 GiB");
    expect(block).toHaveTextContent("Proxy no cap · 0.5 GiB");
  });

  it("says when nothing is holding capacity", async () => {
    getMock.mockResolvedValue(response({ totals: { basis: "requests", ...sums() }, by_runner: { k8s: { basis: "requests", ...sums() } } }));
    renderCard();
    await waitFor(() => expect(screen.getByTestId("fleet-capacity-card")).toHaveTextContent("No runs are holding capacity"));
  });

  it("lists the top 5 owners with Show all, and says when the server truncated", async () => {
    const by_owner = Array.from({ length: 8 }, (_, i) => ({
      owner: `owner${i}@example.com`,
      holding: 1,
      by_runner: { k8s: sums({ holding: 1, held_cpu_millis: 1000, held_memory_mib: 1024 }) },
    }));
    getMock.mockResolvedValue(response({ by_owner, by_owner_truncated: true }));
    renderCard();
    await expand();
    const owners = screen.getByTestId("fleet-capacity-owners");
    expect(within(owners).getAllByRole("row")).toHaveLength(1 + 5);
    expect(owners).toHaveTextContent("Only the top 50 owners by CPU are listed.");
    await userEvent.click(within(owners).getByRole("button", { name: "Show all 8" }));
    expect(within(owners).getAllByRole("row")).toHaveLength(1 + 8);
    expect(within(owners).queryByRole("button", { name: /Show all/ })).toBeNull();
  });
});
