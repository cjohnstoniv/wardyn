/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// FUG-L404: My pools is a card of its own when My runner is off the page because runners are off. The
// strings are written out, not read from the copy modules, so a drift in them fails this suite.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { RunnerPool, RunnerPoolCatalogueEntry } from "../../../lib/types/runner-pools";

const pools = { list: vi.fn(), get: vi.fn(), members: vi.fn() };
const remoteMineMock = vi.fn();
vi.mock("../../../lib/api/runner-pools", () => ({
  runnerPools: new Proxy({}, { get: (_t, k: string) => (...a: unknown[]) => (pools as Record<string, (...x: unknown[]) => unknown>)[k](...a) }),
  remoteRunners: { listMine: (...a: unknown[]) => remoteMineMock(...a) },
}));
const listMineMock = vi.fn();
vi.mock("../../../lib/api/runners", () => ({
  RUNNERS_DISABLED: "runners_disabled",
  runners: { listMine: (...a: unknown[]) => listMineMock(...a) },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import { OperatorProvider } from "../../wardyn/operator-context";
import { MyRunnerAndPoolsCards } from "./my-pools-card";

const POOL = (id: string, name: string): RunnerPool => ({
  id, name, scope: "user", owner: "alice", state: "active", revision: 2, limits: { barriers: ["CC1"], background: {}, interactive: {} }, created_at: aheadByHours(-90), updated_at: aheadByHours(-2),
});
const ENTRY = (p: RunnerPool): RunnerPoolCatalogueEntry => ({ id: p.id, name: p.name, scope: "user", owner: "alice", availability: "available", run_types: ["background", "interactive"], barrier_options: [] });
const BY_ID: Record<string, RunnerPool> = { "p-a": POOL("p-a", "Remote only"), "p-b": POOL("p-b", "Spare") };

function renderCards(hash = "") {
  return render(
    <MemoryRouter initialEntries={[`/account${hash}`]}>
      <OperatorProvider operator={false} securityOperator={false} operatorResolved principal="alice">
        <MyRunnerAndPoolsCards />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  for (const m of [...Object.values(pools), remoteMineMock, listMineMock]) m.mockReset();
  pools.list.mockResolvedValue({ pools: [ENTRY(BY_ID["p-a"]), ENTRY(BY_ID["p-b"])] });
  pools.get.mockImplementation(async (id: string) => BY_ID[id]);
  pools.members.mockResolvedValue([]);
  remoteMineMock.mockResolvedValue([]);
});
afterEach(() => cleanup());

describe("My pools card (FUG-L404)", () => {
  it("is drawn when runners are off, so a person with only remote-hosted pools can manage them", async () => {
    listMineMock.mockRejectedValue(new HttpError(403, "runners are off", "runners_disabled"));
    const user = userEvent.setup();
    renderCards();
    const card = await screen.findByRole("button", { name: /^My pools\s*2 pools/ });
    expect(screen.queryByText("My runner")).toBeNull();
    await user.click(card);
    expect(await screen.findByText("Remote only")).toBeInTheDocument();
    expect(screen.getByText("Spare")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create a pool" })).toBeInTheDocument();
    // The card carries the title once: the section inside has no heading of its own.
    expect(screen.getAllByText("My pools")).toHaveLength(1);
  });

  it("says no pools in its summary, and one pool in the singular", async () => {
    listMineMock.mockRejectedValue(new HttpError(403, "runners are off", "runners_disabled"));
    pools.list.mockResolvedValue({ pools: [] });
    const { unmount } = renderCards();
    expect(await screen.findByRole("button", { name: /^My pools\s*No pools/ })).toBeInTheDocument();
    unmount();
    pools.list.mockResolvedValue({ pools: [ENTRY(BY_ID["p-a"])] });
    renderCards();
    expect(await screen.findByRole("button", { name: /^My pools\s*1 pool$/ })).toBeInTheDocument();
  });

  it("opens and focuses from the link New Run has always used, #my-runner", async () => {
    listMineMock.mockRejectedValue(new HttpError(403, "runners are off", "runners_disabled"));
    renderCards("#my-runner");
    const section = (await screen.findByRole("button", { name: /^My pools/ })).closest("section") as HTMLElement;
    await waitFor(() => expect(section).toHaveFocus());
    expect(await screen.findByText("Remote only")).toBeInTheDocument();
  });

  it("is not drawn when the server manages no pools", async () => {
    listMineMock.mockRejectedValue(new HttpError(403, "runners are off", "runners_disabled"));
    pools.list.mockRejectedValue(new HttpError(501, "no pools"));
    const { container } = renderCards();
    await waitFor(() => expect(pools.list).toHaveBeenCalled());
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it("is not drawn for a credential that is not a person's session", async () => {
    listMineMock.mockRejectedValue(new HttpError(403, "not a session", "runner_session_required"));
    const { container } = renderCards();
    await waitFor(() => expect(listMineMock).toHaveBeenCalled());
    await waitFor(() => expect(container).toBeEmptyDOMElement());
    expect(pools.list).not.toHaveBeenCalled();
  });

  it("is not a second card when My runner is drawn: the section stays inside it", async () => {
    listMineMock.mockResolvedValue([]);
    renderCards();
    expect(await screen.findByRole("button", { name: /^My runner/ })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^My pools/ })).toBeNull();
  });
});
