/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Review and the kept machines (board KM-2): a draft that keeps or resumes a machine is held, with a plain sentence,
// while the read of the person's kept machines has failed, and is let go once the read answers; the choice stays throughout.

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { OperatorProvider } from "../../../wardyn/operator-context";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { KEEP_NEW_RUN } from "../../../wardyn/copy/keep-machine";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import type { RunIntent } from "../draft/run-intent";
import { ModelAccessProvider } from "../../../wardyn/model-access-context";
import type { SetupStatus } from "../../../../lib/types/setup";
import type { MyMachines } from "../../../../lib/types";
import { HttpError } from "../../../../lib/api/core";
import { NEW_RUN_SCREENS, NewRunRoute, useNewRunDispatch, type Seed, type SeedLoaders } from "../new-run-route";
import { ReviewScreen } from "./review-screen";

const { checks, bodies, wfetchMock, listMock } = vi.hoisted(() => ({
  checks: { current: {} as Record<string, unknown> },
  bodies: [] as unknown[],
  wfetchMock: vi.fn(),
  listMock: vi.fn(),
}));
vi.mock("../../../../lib/api/me-machines", () => ({ myMachines: () => listMock() }));
vi.mock("../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 1, authGeneration: 0 }),
  useIntentChecks: (p: { body: unknown }) => {
    bodies.push(p.body);
    return checks.current;
  },
}));
vi.mock("../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

const RUN_ID = "11111111-1111-4111-8111-111111111111";
const INERT = Object.fromEntries(Object.keys(NEW_RUN_SCREENS).map((name) => [name, () => null])) as unknown as typeof NEW_RUN_SCREENS;
const MAC: RunIntent = {
  shape: "agent_task", experience: "interactive", runner_pool_id: "pool-general", no_repositories_or_drives: true,
  workload: { kind: "agent_task", agent: "claude-code" },
  tools: [{ id: "claude-code", kind: "harness" }],
  machine_kind: "macos_vm",
};
const MACHINE = { resume: "m-dev" };
const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });

// A rerun never carries a machine (it is chosen at each launch), so the choice is made in the draft once the page is up.
function WithMachine({ children }: { children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const [done, setDone] = React.useState(false);
  React.useEffect(() => {
    dispatch({ type: "edit", apply: (i) => ({ ...i, machine: MACHINE }) });
    setDone(true);
  }, [dispatch]);
  return done ? children : null;
}

const ON = { features: { macos_vms: true } } as SetupStatus;
const LIST: MyMachines = { machines: [], allowed: true, count: 0, limit: 1, worst_bytes: 0 };

function open(intent: RunIntent) {
  const loaders: SeedLoaders = {
    template: async (): Promise<Seed> => { throw new Error("no template"); },
    again: async (): Promise<Seed> => ({ origin: { kind: "again", runId: RUN_ID, title: "Nightly" }, intent }),
  };
  wfetchMock.mockImplementation(async (path: string) => {
    if (path.startsWith("/me/drives")) return json({ drives: [], pool_checked: true });
    if (path === "/me/walls") return json({ spec: {} });
    if (path === "/templates") return json({ templates: [], publish: { org: false, groups: [], groups_unverified: false } });
    throw new Error(`unexpected ${path}`);
  });
  render(
    <MemoryRouter initialEntries={[`/runs/new?again=${RUN_ID}`]}>
      <OperatorProvider operator={false} principal="alice">
        <ModelAccessProvider status={ON} onRefresh={() => {}}>
          <Routes>
            <Route path="/runs/new" element={<NewRunRoute loaders={loaders} screens={{ ...INERT, review: (p) => <WithMachine><ReviewScreen {...p} /></WithMachine> }} />} />
          </Routes>
        </ModelAccessProvider>
      </OperatorProvider>
    </MemoryRouter>,
  );
}
const answered = () => ({
  generation: 5,
  preflightRead: { generation: 5, at: Date.now() - 1000 },
  preflight: vi.fn(),
  preflightResult: SCENARIO_PREFLIGHT.member,
  preflightError: null,
  preflightReason: "",
  preflightField: "",
  preview: { result: null, retry: vi.fn() },
});
const blockerList = () => document.getElementById("nr-blockers") as HTMLElement;

beforeEach(() => {
  listMock.mockReset();
  listMock.mockResolvedValue(LIST);
  bodies.length = 0;
});
afterEach(cleanup);

describe("Review: the kept machines read", () => {
  it("a failed read holds Launch with the plain sentence, and the draft keeps its resume", async () => {
    checks.current = answered();
    listMock.mockRejectedValue(new HttpError(500, "boom"));
    open(MAC);
    await waitFor(() => expect(within(blockerList()).getByText(KEEP_NEW_RUN.READ_FAILED)).toBeInTheDocument());
    expect(screen.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toHaveAttribute("aria-disabled", "true");
    expect(bodies[bodies.length - 1]).toMatchObject({ machine: { resume: "m-dev" } });
  });

  it("a read still loading does not hold Launch", async () => {
    checks.current = answered();
    listMock.mockReturnValue(new Promise(() => {}));
    open(MAC);
    await screen.findByRole("button", { name: NR_REVIEW.LAUNCH_RUN });
    expect(screen.queryByText(KEEP_NEW_RUN.READ_FAILED)).toBeNull();
  });
});
