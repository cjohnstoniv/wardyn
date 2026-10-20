/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Admin -> Runners Pools tab (mock M-POOLS v6 A1, A8, A9). The strings are written out, not read from the
// copy module, so a drift in copy/runner-pools.ts fails this suite.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { RunnerPool } from "../../../../lib/types/runner-pools";

const listMock = vi.fn();
const getMock = vi.fn();
const orgDefaultsMock = vi.fn();
const setOrgDefaultsMock = vi.fn();
const usePolicyMock = vi.fn();
vi.mock("../../../../lib/api/runner-pools", () => ({
  runnerPools: {
    list: (...a: unknown[]) => listMock(...a),
    get: (...a: unknown[]) => getMock(...a),
    orgDefaults: (...a: unknown[]) => orgDefaultsMock(...a),
    setOrgDefaults: (...a: unknown[]) => setOrgDefaultsMock(...a),
    usePolicy: (...a: unknown[]) => usePolicyMock(...a),
  },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import * as React from "react";
import { HttpError } from "../../../../lib/api/core";
import { OperatorProvider } from "../../../wardyn/operator-context";
import { entry, pool, remoteRunner, selfRunner } from "./pool-test-fixtures";
import { PoolsTab } from "./pools-tab";

const GENERAL = pool({ id: "p-gen", name: "General", limits: { barriers: ["CC1", "CC2", "CC3"], max_concurrent_runs: 10 } });
const CI = pool();
const STAGING = pool({ id: "p-stg", name: "Staging", state: "disabled", limits: { interactive: {}, barriers: ["CC1", "CC2"], max_concurrent_runs: 3 } });
const LAPTOP = pool({ id: "p-laptop", name: "Laptop", scope: "user", owner: "dana@example.com", limits: { barriers: ["CC1", "CC2"] } });
const ALL = [GENERAL, CI, STAGING, LAPTOP];

const EU = remoteRunner({ pools: [{ id: "p-gen", name: "General", scope: "organization" }, { id: "p-ci", name: "CI jobs", scope: "organization" }] });
const SHARED = remoteRunner({ id: "rr-sh", name: "shared-docker", pools: [{ id: "p-laptop", name: "Laptop", scope: "user", owner: "dana@example.com" }] });
const LAP = selfRunner({ pools: [{ id: "p-laptop", name: "Laptop" }] });

function Harness({ onOpenRunner = vi.fn() }: { onOpenRunner?: (k: string, id: string) => void }) {
  const [open, setOpen] = React.useState<string | null>(null);
  return <PoolsTab runners={[LAP]} remote={[EU, SHARED]} openPoolId={open} onOpenPool={setOpen} onOpenRunner={onOpenRunner} />;
}

function renderTab(operator = true) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator={operator} securityOperator operatorResolved principal="dana@example.com">
        <Harness />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  for (const m of [listMock, getMock, orgDefaultsMock, setOrgDefaultsMock, usePolicyMock]) m.mockReset();
  listMock.mockResolvedValue({ pools: ALL.map((p) => entry(p)) });
  getMock.mockImplementation((id: string) => Promise.resolve(ALL.find((p) => p.id === id) as RunnerPool));
  orgDefaultsMock.mockResolvedValue({ pool_id: "p-gen", barrier: "CC2" });
  setOrgDefaultsMock.mockResolvedValue({});
  usePolicyMock.mockResolvedValue({ pool_id: "p-ci", subjects: [], revision: 1, updated_at: "0001-01-01T00:00:00Z" });
});
afterEach(() => cleanup());

const orgTable = () => screen.getByRole("table", { name: "Organization pools" });
const userTable = () => screen.getByRole("table", { name: "User pools" });

describe("Pools tab", () => {
  it("shows organization pools and user pools, each with what it allows, its runners and its state", async () => {
    renderTab();
    await screen.findByRole("table", { name: "Organization pools" });
    expect(screen.getByRole("heading", { name: "Organization pools", level: 2 })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "User pools", level: 2 })).toBeInTheDocument();
    expect(screen.getByText("wardyn runner pool create --scope organization --runner <runner>")).toBeInTheDocument();
    expect(screen.getByText(/^Pools your organization provides/)).toBeInTheDocument();
    expect(screen.getByText(/You edit them here\.$/)).toBeInTheDocument();
    expect(screen.queryByText("No organization pools yet")).toBeNull();
    expect(screen.getByRole("link", { name: "Governance" })).toHaveAttribute("href", "/admin/governance");
    const org = within(orgTable());
    expect(org.getAllByRole("row")).toHaveLength(4);
    expect(org.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Pool", "Allows", "Runners", "State", ""]);
    expect(org.getAllByText("Interactive and background").length).toBeGreaterThan(0);
    expect(org.getByText("Pool limits: 10 runs maximum")).toBeInTheDocument();
    expect(org.getAllByText("6 running · 2 waiting")).toHaveLength(3);
    expect(org.getAllByText("eu-cluster")).toHaveLength(2);
    const user = within(userTable());
    expect(user.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Pool", "Owner", "Allows", "Runners", "State"]);
    expect(user.getByText("dana@example.com")).toBeInTheDocument();
    expect(user.getByText("laptop-7")).toBeInTheDocument();
    expect(user.getByText(/shared-docker/)).toHaveTextContent("shared-docker · remote-hosted");
  });

  it("marks the default with a chip and no button, and offers Make default on the others a super admin may choose", async () => {
    renderTab();
    await screen.findByRole("table", { name: "Organization pools" });
    const rows = within(orgTable()).getAllByRole("row").slice(1);
    expect(within(rows[0]).getByText("✓ Default")).toBeInTheDocument();
    expect(within(rows[0]).queryByRole("button", { name: /Make/ })).toBeNull();
    expect(within(rows[1]).getByRole("button", { name: "Make CI jobs the organization default" })).toHaveTextContent("Make default");
    expect(within(rows[2]).getByText("Can't be the default while it's switched off")).toBeInTheDocument();
    expect(within(rows[2]).getByText("Switched off")).toBeInTheDocument();
    expect(screen.getByText("The default seeds each person's New Run when they choose an organization pool. It grants nobody a pool.")).toBeInTheDocument();
  });

  it("tells a security admin only a super admin changes organization pools, and offers no Make default", async () => {
    renderTab(false);
    await screen.findByRole("table", { name: "Organization pools" });
    expect(screen.getByText("Only a super admin can change organization pools. You can set who may use one.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Make .* the organization default/ })).toBeNull();
  });

  it("says so when there is no organization default", async () => {
    orgDefaultsMock.mockResolvedValue({});
    renderTab();
    await screen.findByText("No organization default. People choose an organization pool each time.");
    expect(screen.queryByText("✓ Default")).toBeNull();
  });

  it("shows the empty states when there are no pools", async () => {
    listMock.mockResolvedValue({ pools: [] });
    renderTab();
    expect(await screen.findByText("No organization pools yet")).toBeInTheDocument();
    const empty = screen.getByRole("group", { name: "No organization pools yet" });
    expect(empty).toHaveTextContent("Organization pools are created on the command line:");
    expect(within(empty).getByText("wardyn runner pool create --scope organization --runner <runner>")).toBeInTheDocument();
    expect(within(empty).getByRole("button", { name: "Copy" })).toBeInTheDocument();
    expect(empty).toHaveTextContent("It then appears here.");
    expect(empty).not.toHaveTextContent("as above");
    expect(screen.getByRole("heading", { name: "Organization pools", level: 2 })).toBeInTheDocument();
    expect(screen.queryByText(/^Pools your organization provides/)).toBeNull();
    expect(screen.queryByText(/You edit them here/)).toBeNull();
    expect(screen.getByText("No user pools yet")).toBeInTheDocument();
    expect(screen.getByText("People create their own from Your account → My runner.")).toBeInTheDocument();
  });

  it("announces loading, then the pools", async () => {
    let release: (v: unknown) => void = () => {};
    listMock.mockReturnValue(new Promise((r) => (release = r)));
    renderTab();
    expect(screen.getByRole("status", { name: "Loading pools" })).toBeInTheDocument();
    release({ pools: [entry(CI)] });
    expect(await screen.findByRole("table", { name: "Organization pools" })).toBeInTheDocument();
    expect(screen.queryByRole("status", { name: "Loading pools" })).toBeNull();
  });

  it("shows a failed read with Retry, and Retry reads again", async () => {
    listMock.mockRejectedValueOnce(new Error("boom"));
    renderTab();
    expect(await screen.findByText("Couldn't load pools")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("table", { name: "Organization pools" })).toBeInTheDocument();
    expect(listMock).toHaveBeenCalledTimes(2);
  });

  it("shows the server's own sentence when pools are unavailable (501)", async () => {
    listMock.mockRejectedValue(new HttpError(501, "This server does not manage runner pools yet.", "runner_pools_unavailable"));
    renderTab();
    expect(await screen.findByText("Runner pools are unavailable")).toBeInTheDocument();
    expect(screen.getByText(/This server does not manage runner pools yet\./)).toBeInTheDocument();
    expect(screen.queryByText("No organization pools yet")).toBeNull();
  });

  it("makes a pool the default and keeps the organization's barrier", async () => {
    renderTab();
    await userEvent.click(await screen.findByRole("button", { name: "Make CI jobs the organization default" }));
    await waitFor(() => expect(setOrgDefaultsMock).toHaveBeenCalledWith({ pool_id: "p-ci", barrier: "CC2" }));
  });

  it("keeps the organization's recording setting and names the revision it read when it makes a pool the default", async () => {
    orgDefaultsMock.mockResolvedValue({ pool_id: "p-gen", barrier: "CC2", recording: "required", revision: 4 });
    renderTab();
    await userEvent.click(await screen.findByRole("button", { name: "Make CI jobs the organization default" }));
    await waitFor(() => expect(setOrgDefaultsMock).toHaveBeenCalledWith({ pool_id: "p-ci", barrier: "CC2", recording: "required", revision: 4 }));
  });

  it("shows the server's sentence above the table when the default is refused", async () => {
    setOrgDefaultsMock.mockRejectedValue(new HttpError(409, "CI jobs is switched off right now. Choose another pool.", "runner_pool_unavailable"));
    renderTab();
    await userEvent.click(await screen.findByRole("button", { name: "Make CI jobs the organization default" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("CI jobs is switched off right now. Choose another pool.");
  });

  it("opens a pool's details from its name and from Details", async () => {
    renderTab();
    await userEvent.click(await screen.findByRole("button", { name: "Details of CI jobs" }));
    expect(await screen.findByRole("dialog")).toHaveTextContent("CI jobs");
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await userEvent.click(within(orgTable()).getByRole("button", { name: "General" }));
    expect(await screen.findByRole("dialog")).toHaveTextContent("This is the organization default.");
  });

  // The list grows with people, so the per-pool reads are bounded: every pool once when the tab opens, and
  // after that a re-read asks only for the list, the default and the pool whose details are open.
  it("reads every pool once, then re-reads only the list and the open pool", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    try {
      renderTab();
      await screen.findByRole("table", { name: "Organization pools" });
      expect(getMock.mock.calls.map(([id]) => id).sort()).toEqual(ALL.map((p) => p.id).sort());
      const tick = async () => {
        const lists = listMock.mock.calls.length;
        await act(async () => {
          vi.advanceTimersByTime(30_000);
        });
        await waitFor(() => expect(listMock.mock.calls.length).toBe(lists + 1));
      };
      await tick();
      expect(getMock).toHaveBeenCalledTimes(ALL.length);
      // With a pool's details open, that pool alone is read again on each tick, so a change to it shows.
      await userEvent.click(screen.getByRole("button", { name: "Details of CI jobs" }));
      await waitFor(() => expect(getMock).toHaveBeenCalledTimes(ALL.length + 1));
      getMock.mockImplementation((id: string) => Promise.resolve({ ...(ALL.find((p) => p.id === id) as RunnerPool), state: "disabled" }));
      await tick();
      expect(getMock).toHaveBeenCalledTimes(ALL.length + 2);
      expect(getMock).toHaveBeenLastCalledWith("p-ci");
      expect(await within(screen.getByRole("dialog")).findByText("Switched off")).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  // The sheet's state and revision are what Switch off sends back, so they are read when it opens.
  it("reads a pool when its details open, without waiting for the next re-read", async () => {
    renderTab();
    await screen.findByRole("table", { name: "Organization pools" });
    expect(getMock).toHaveBeenCalledTimes(ALL.length);
    getMock.mockImplementation((id: string) => Promise.resolve({ ...(ALL.find((p) => p.id === id) as RunnerPool), state: "disabled" }));
    await userEvent.click(screen.getByRole("button", { name: "Details of CI jobs" }));
    expect(await within(await screen.findByRole("dialog")).findByText("Switched off")).toBeInTheDocument();
    expect(getMock).toHaveBeenCalledTimes(ALL.length + 1);
    expect(getMock).toHaveBeenLastCalledWith("p-ci");
  });

  it("reads a long list four pools at a time", async () => {
    const many = Array.from({ length: 10 }, (_, i) => pool({ id: `p-${i}`, name: `Pool ${i}` }));
    listMock.mockResolvedValue({ pools: many.map((p) => entry(p)) });
    let inFlight = 0;
    let widest = 0;
    getMock.mockImplementation(async (id: string) => {
      widest = Math.max(widest, ++inFlight);
      await new Promise((r) => setTimeout(r, 5));
      inFlight--;
      return many.find((p) => p.id === id) as RunnerPool;
    });
    renderTab();
    expect(await within(await screen.findByRole("table", { name: "Organization pools" })).findAllByRole("row")).toHaveLength(11);
    expect(getMock).toHaveBeenCalledTimes(10);
    expect(widest).toBe(4);
  });
});
