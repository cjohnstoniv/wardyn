/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Review with a Later start the switch, off or not known, holds: the Run and timing row needs attention, so the Run
// setup header says so too, never Complete with the check above it. Launch run stays open: the Name dialog holds it.

import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ModelAccessProvider } from "../../../wardyn/model-access-context";
import { OperatorProvider } from "../../../wardyn/operator-context";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import type { StartChoice } from "../../../../lib/schedule";
import type { SetupStatus } from "../../../../lib/types/setup";
import type { RunIntent } from "../draft/run-intent";
import { NEW_RUN_SCREENS, NewRunRoute, useNewRunDispatch, useNewRunDraft, type Seed, type SeedLoaders } from "../new-run-route";
import { ReviewScreen } from "./review-screen";

const { wfetchMock } = vi.hoisted(() => ({ wfetchMock: vi.fn() }));
vi.mock("../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 1, authGeneration: 0 }),
  useIntentChecks: () => ({
    generation: 5,
    preflightRead: { generation: 5, at: Date.now() - 1000 },
    preflight: vi.fn(),
    preflightResult: SCENARIO_PREFLIGHT.member,
    preflightError: null,
    preflightReason: "",
    preview: { result: null, retry: vi.fn() },
  }),
}));
vi.mock("../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

afterEach(cleanup);

const RUN_ID = "11111111-1111-4111-8111-111111111111";
const INERT = Object.fromEntries(Object.keys(NEW_RUN_SCREENS).map((name) => [name, () => null])) as unknown as typeof NEW_RUN_SCREENS;
const AUTONOMOUS: RunIntent = {
  shape: "agent_task", experience: "background", runner_pool_id: "pool-general", no_repositories_or_drives: true,
  workload: { kind: "agent_task", agent: "claude-code", task: "Fix the build" },
  tools: [{ id: "claude-code", kind: "harness" }],
};
const LATER: StartChoice = { date: new Date(Date.now() + 86_400_000).toISOString().slice(0, 10), time: "02:00", zone: "UTC" };
const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });

// The Later start, set once the seed is in: seeding clears the draft's schedule.
function Later() {
  const dispatch = useNewRunDispatch();
  const seeded = useNewRunDraft().origin.kind !== "scratch";
  React.useEffect(() => {
    if (seeded) dispatch({ type: "schedule", schedule: LATER });
  }, [seeded, dispatch]);
  return null;
}

function open(status: SetupStatus) {
  const loaders: SeedLoaders = {
    template: async (): Promise<Seed> => { throw new Error("no template"); },
    again: async (): Promise<Seed> => ({ origin: { kind: "again", runId: RUN_ID, title: "Nightly" }, intent: AUTONOMOUS }),
  };
  wfetchMock.mockImplementation(async (path: string) => {
    if (path.startsWith("/me/drives")) return json({ drives: [], pool_checked: true });
    if (path === "/me/walls") return json({ spec: {} });
    if (path === "/templates") return json({ templates: [], publish: { org: false, groups: [], groups_unverified: false } });
    throw new Error(`unexpected ${path}`);
  });
  render(
    <ModelAccessProvider status={status} onRefresh={() => {}}>
      <MemoryRouter initialEntries={[`/runs/new?again=${RUN_ID}`]}>
        <OperatorProvider operator={false} principal="alice">
          <Routes>
            <Route path="/runs/new" element={<NewRunRoute loaders={loaders} screens={{ ...INERT, review: (p) => <><ReviewScreen {...p} /><Later /></> }} />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>
    </ModelAccessProvider>,
  );
}
const runRow = () => within(document.querySelector("[data-review-group=run]") as HTMLElement);
const launch = () => screen.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN });

describe("Review: a held start", () => {
  it("marks Run setup as needing attention, never Complete, while the switch is off or not known", async () => {
    for (const status of [{ features: { scheduled_runs: false } }, { unreachable: true }] as SetupStatus[]) {
      open(status);
      await waitFor(() => expect(runRow().getByText(NR_REVIEW.BADGE_ATTENTION)).toBeInTheDocument());
      const mark = screen.getByTestId("review-status-mark");
      expect(mark).toHaveAttribute("data-tone", "danger");
      expect(mark).toHaveTextContent(NR_REVIEW.BADGE_ATTENTION);
      expect(launch()).toBeEnabled();
      cleanup();
    }
  });

  it("stays Complete when the switch is on and keeps the start", async () => {
    open({ features: { scheduled_runs: true } } as SetupStatus);
    await waitFor(() => expect(runRow().getByText(NR_REVIEW.TIMING_CHECKED, { exact: false })).toBeInTheDocument());
    expect(runRow().queryByText(NR_REVIEW.BADGE_ATTENTION)).not.toBeInTheDocument();
    expect(screen.getByTestId("review-status-mark")).toHaveAttribute("data-tone", "success");
    expect(launch()).toBeEnabled();
  });
});
