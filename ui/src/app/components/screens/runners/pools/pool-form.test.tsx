/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pool form (mock M-POOLS v6 A3, B10, B11). The strings are written out, not read from the copy module,
// so a drift in copy/runner-pools.ts fails this suite.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const getMock = vi.fn();
const membersMock = vi.fn();
const createMineMock = vi.fn();
const updateMineMock = vi.fn();
const updateMock = vi.fn();
vi.mock("../../../../lib/api/runner-pools", () => ({
  runnerPools: {
    get: (...a: unknown[]) => getMock(...a),
    members: (...a: unknown[]) => membersMock(...a),
    createMine: (...a: unknown[]) => createMineMock(...a),
    updateMine: (...a: unknown[]) => updateMineMock(...a),
    update: (...a: unknown[]) => updateMock(...a),
  },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { HttpError } from "../../../../lib/api/core";
import { setField } from "../../../../../test/set-field";
import { member, pool, remoteRunner } from "./pool-test-fixtures";
import { PoolFormScreen } from "./pool-form";

const MINE = pool({ id: "p-lap", name: "Laptop", scope: "user", owner: "dana@example.com", limits: { background: { lifetime: { default_sec: 7200, max_sec: 43200 } }, barriers: ["CC1", "CC2"] } });

function renderAt(path: string, scope: "admin" | "mine") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/account/pools/new" element={<PoolFormScreen scope={scope} />} />
        <Route path="/account/pools/:id/edit" element={<PoolFormScreen scope={scope} />} />
        <Route path="/admin/runners/pools/:id/edit" element={<PoolFormScreen scope={scope} />} />
        <Route path="/account" element={<p>account page</p>} />
        <Route path="/admin/runners" element={<p>runners page</p>} />
      </Routes>
    </MemoryRouter>,
  );
}

const field = (name: string) => screen.getByLabelText(name);

beforeEach(() => {
  for (const m of [getMock, membersMock, createMineMock, updateMineMock, updateMock]) m.mockReset();
  getMock.mockResolvedValue(MINE);
  membersMock.mockResolvedValue([]);
});
afterEach(() => cleanup());

describe("pool form layout", () => {
  // #2894: the back link sits above the title and lines up with the page edge (no sm padding inset).
  it("puts the back link above the title, flush to the left edge", async () => {
    renderAt("/account/pools/new", "mine");
    const title = await screen.findByRole("heading", { level: 1 });
    const back = screen.getByRole("button", { name: "← Back to My runner" });
    expect(back.compareDocumentPosition(title) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(back).toHaveClass("-ml-3");
  });

  // #3063: the qualifier is said once per fieldset, not under every resource field.
  it("says the qualifier once per fieldset, with the not-chosen sentence appended while no runners are chosen", async () => {
    renderAt("/account/pools/new", "mine");
    await screen.findByRole("heading", { level: 1 });
    const once = "Optional. Leave empty to follow its runners, and it can only narrow them. Its runners aren't chosen yet.";
    expect(screen.getByTestId("f-bg-qualifier")).toHaveTextContent(once);
    expect(screen.getByTestId("f-it-qualifier")).toHaveTextContent(once);
    expect(screen.getAllByText(/aren't chosen yet/)).toHaveLength(2);
    expect(screen.getAllByText(/narrower than its runners, optional/)).toHaveLength(1);
  });
});

describe("new user pool", () => {
  it("creates the pool with wire units and goes back to My runner", async () => {
    createMineMock.mockResolvedValue({});
    renderAt("/account/pools/new", "mine");
    expect(screen.getByRole("heading", { name: "New user pool", level: 1 })).toBeInTheDocument();
    expect(screen.getByText("(yours: only you can use it, change it or choose its runners)", { exact: false })).toBeInTheDocument();
    setField(field("Name"), "Garage server");
    setField(screen.getByRole("spinbutton", { name: /Runs at once in this pool/ }), "3");
    setField(field("Background tasks, CPU (CPUs), Default"), "1");
    setField(field("Background tasks, CPU (CPUs), Maximum per run"), "4");
    setField(field("Background tasks, Memory (GB), Default"), "2");
    setField(field("Background tasks, Memory (GB), Maximum per run"), "8");
    setField(field("Background tasks, Disk (GB), Default"), "20");
    setField(field("Background tasks, Disk (GB), Maximum per run"), "100");
    await userEvent.click(screen.getByRole("button", { name: "Create pool" }));
    await waitFor(() => expect(createMineMock).toHaveBeenCalledTimes(1));
    expect(createMineMock).toHaveBeenCalledWith({
      name: "Garage server",
      limits: {
        barriers: ["CC1", "CC2"],
        max_concurrent_runs: 3,
        background: {
          cpu_millis: { default: 1000, cap: 4000 },
          memory_mib: { default: 2048, cap: 8192 },
          disk_mib: { default: 20480, cap: 102400 },
          lifetime: { default_sec: 3600, max_sec: 86400 },
        },
        interactive: { lifetime: { default_sec: 28800, max_sec: 86400 }, idle: { default_sec: 1800, max_sec: 7200 } },
      },
    });
    expect(await screen.findByText("account page")).toBeInTheDocument();
  });

  it("refuses an empty name, a missing half of an amount and fractional disk, and sends nothing", async () => {
    renderAt("/account/pools/new", "mine");
    await userEvent.click(screen.getByRole("button", { name: "Create pool" }));
    expect(screen.getAllByRole("alert")[0]).toHaveTextContent("Name the pool in 1 to 64 characters, without leading or trailing spaces.");
    expect(field("Name")).toHaveAttribute("aria-invalid", "true");
    setField(field("Name"), "Laptop");
    setField(field("Background tasks, CPU (CPUs), Default"), "2");
    setField(field("Background tasks, Disk (GB), Default"), "10.5");
    setField(field("Background tasks, Disk (GB), Maximum per run"), "20");
    await userEvent.click(screen.getByRole("button", { name: "Create pool" }));
    expect(screen.getByText("Background tasks, CPU: set both a default and a maximum, or leave both empty.", { selector: "p[id]" })).toBeInTheDocument();
    expect(screen.getByText("Background tasks, disk: set whole numbers of GB.", { selector: "p[id]" })).toBeInTheDocument();
    expect(createMineMock).not.toHaveBeenCalled();
  });

  it("needs a run type and a barrier", async () => {
    renderAt("/account/pools/new", "mine");
    setField(field("Name"), "Laptop");
    await userEvent.click(screen.getByRole("checkbox", { name: "Background tasks" }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Interactive environments" }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Fence" }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Wall" }));
    await userEvent.click(screen.getByRole("button", { name: "Create pool" }));
    expect(screen.getByText("A pool allows at least one run type: choose background, interactive or both.", { selector: "p[role=alert]" })).toBeInTheDocument();
    expect(screen.getByText("A pool allows at least one barrier.", { selector: "p[role=alert]" })).toBeInTheDocument();
  });
});

describe("edit", () => {
  it("prefills the pool, saves with its revision and goes back", async () => {
    updateMineMock.mockResolvedValue({});
    renderAt("/account/pools/p-lap/edit", "mine");
    expect(await screen.findByRole("heading", { name: "Edit Laptop", level: 1 })).toBeInTheDocument();
    expect(field("Name")).toHaveValue("Laptop");
    expect(field("Background tasks, Lifetime, Default")).toHaveValue(2);
    expect(screen.getByRole("checkbox", { name: "Interactive environments" })).not.toBeChecked();
    expect(screen.queryByText("Interactive environments", { selector: "legend" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(updateMineMock).toHaveBeenCalledWith("p-lap", {
        revision: 9,
        name: "Laptop",
        limits: { barriers: ["CC1", "CC2"], background: { lifetime: { default_sec: 7200, max_sec: 43200 } } },
      }),
    );
    expect(await screen.findByText("account page")).toBeInTheDocument();
  });

  // The form shows durations exactly (seconds, minutes, hours or days) and an unset default as 1 hour; a rename sends the pool's
  // durations as they were read.
  it("a rename sends 90 s and an idle stop that never stops by default exactly as read", async () => {
    const limits = {
      background: { lifetime: { default_sec: 90, max_sec: 150 } },
      interactive: { idle: { default_sec: 0, unlimited: true } },
      barriers: ["CC1" as const],
    };
    getMock.mockResolvedValue({ ...MINE, limits });
    updateMineMock.mockResolvedValue({});
    renderAt("/account/pools/p-lap/edit", "mine");
    expect(await screen.findByRole("heading", { name: "Edit Laptop", level: 1 })).toBeInTheDocument();
    setField(field("Name"), "Laptop 2");
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(updateMineMock).toHaveBeenCalledWith("p-lap", { revision: 9, name: "Laptop 2", limits }));
  });

  it("an organization pool saves through the organization route and goes back to the pools", async () => {
    getMock.mockResolvedValue(pool());
    membersMock.mockResolvedValue([member(remoteRunner())]);
    updateMock.mockResolvedValue({});
    renderAt("/admin/runners/pools/p-ci/edit", "admin");
    expect(await screen.findByRole("heading", { name: "Edit CI jobs", level: 1 })).toBeInTheDocument();
    expect(screen.getByText("Its runners allow 4 runs at once in total. A run launched past this waits for a free slot.")).toBeInTheDocument();
    expect(screen.getAllByText("Its runners allow up to 4 CPUs per run.")).toHaveLength(1);
    expect(screen.getByTestId("f-bg-qualifier")).toHaveTextContent(/^Optional\. Leave empty to follow its runners, and it can only narrow them\.$/);
    expect(screen.getByText("Organization pools take remote-hosted runners and the organization's own runners.", { exact: false })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(updateMock).toHaveBeenCalledWith("p-ci", expect.objectContaining({ revision: 9, name: "CI jobs" })));
    expect(await screen.findByText("runners page")).toBeInTheDocument();
  });

  it("shows the server's sentence with Reload when the pool changed underneath, and Reload re-reads it into the form", async () => {
    updateMineMock.mockRejectedValue(new HttpError(409, "Laptop changed while this was open.", "runner_pool_stale"));
    renderAt("/account/pools/p-lap/edit", "mine");
    await screen.findByRole("heading", { name: "Edit Laptop" });
    setField(field("Name"), "Laptop 2");
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("Laptop changed while this was open.", { exact: false })).toBeInTheDocument();
    expect(screen.getByText("Reload the pool to see the other change, then redo yours.")).toBeInTheDocument();
    getMock.mockResolvedValue({ ...MINE, name: "Laptop (renamed)", revision: 10 });
    await userEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(field("Name")).toHaveValue("Laptop (renamed)"));
    expect(screen.queryByText("Reload the pool to see the other change, then redo yours.")).toBeNull();
  });

  it("FR-0116: says so when Reload cannot re-read the pool, and keeps Reload", async () => {
    updateMineMock.mockRejectedValue(new HttpError(409, "Laptop changed while this was open.", "runner_pool_stale"));
    renderAt("/account/pools/p-lap/edit", "mine");
    await screen.findByRole("heading", { name: "Edit Laptop" });
    setField(field("Name"), "Laptop 2");
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await screen.findByText("Laptop changed while this was open.", { exact: false });
    getMock.mockRejectedValue(new HttpError(500, "boom"));
    await userEvent.click(screen.getByRole("button", { name: "Reload" }));
    expect(await screen.findByText("Couldn't reload this pool. Your changes are still here. Try Reload again.")).toBeInTheDocument();
    expect(screen.queryByText("Something went wrong reaching the server.", { exact: false })).toBeNull();
    expect(screen.getByRole("button", { name: "Reload" })).toBeInTheDocument();
  });

  it("shows any other refusal as the server's sentence, without Reload", async () => {
    updateMineMock.mockRejectedValue(new HttpError(409, "a pool with that name already exists", "runner_pool_name_taken"));
    renderAt("/account/pools/p-lap/edit", "mine");
    await screen.findByRole("heading", { name: "Edit Laptop" });
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("a pool with that name already exists")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Reload" })).toBeNull();
  });
});

describe("a pool that fails to load", () => {
  it("offers Reload and Back, and Reload shows the form once the read succeeds", async () => {
    getMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    renderAt("/account/pools/p-lap/edit", "mine");
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "← Back to My runner" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Reload" }));
    expect(await screen.findByLabelText("Name")).toHaveValue("Laptop");
  });
});
