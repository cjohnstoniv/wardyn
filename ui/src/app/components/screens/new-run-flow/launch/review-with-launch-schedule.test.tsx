/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A repeating start from Review: the button says Create schedule, the Name dialog posts POST /run-schedules with
// the create request inside and no start time of its own, never POST /runs, and the console lands on Schedules.

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { runs } from "../../../../lib/api/runs";
import { apiURL } from "../../../../lib/base-path";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import type { StartChoice } from "../../../../lib/schedule";
import type { PreflightResult } from "../../../../lib/types/preflight";
import { RUN_SCHEDULE } from "../../../wardyn/copy/run-schedules";
import { NR_LAUNCH } from "../../../wardyn/copy/new-run/launch";
import { OperatorProvider } from "../../../wardyn/operator-context";
import type { RunIntent } from "../draft/run-intent";
import { NEW_RUN_SCREENS, NewRunRoute, useNewRunDispatch, useNewRunDraft, type NewRunScreenProps, type Seed, type SeedLoaders } from "../new-run-route";
import { ReviewWithLaunch } from "./review-with-launch";

vi.mock("../../../wardyn/model-access-context", () => {
  const status = { features: { scheduled_runs: true } };
  return {
    useModelAccessDoor: () => ({ openDoor: vi.fn(), refresh: vi.fn() }),
    useShellSetupStatus: () => ({ status, refresh: vi.fn() }),
  };
});

const RUN_ID = "c3333333-3333-4333-8333-333333333333";
const AUTONOMOUS: RunIntent = {
  shape: "agent_task", experience: "background", runner_pool_id: "pool-general", no_repositories_or_drives: true,
  workload: { kind: "agent_task", agent: "claude-code", task: "nightly report" },
  tools: [{ id: "claude-code", kind: "harness" }],
};
const CLEAR = { ...SCENARIO_PREFLIGHT.member, limits: { lifetime_sec: { default: { value: 3600 }, cap: { value: 7200 } } } } as unknown as PreflightResult;
const WEEKLY: StartChoice = { date: "", time: "09:00", zone: "Europe/London", repeat: "weekly" };
const loaders: SeedLoaders = {
  template: async (): Promise<Seed> => ({ origin: { kind: "scratch" }, intent: AUTONOMOUS }),
  again: async (): Promise<Seed> => ({ origin: { kind: "again", runId: RUN_ID, title: "before" }, intent: AUTONOMOUS }),
};

/** Review with the draft's start set to repeat weekly, as the Run step's Repeat choice leaves it. */
function ReviewRepeating(props: NewRunScreenProps) {
  const dispatch = useNewRunDispatch();
  const draft = useNewRunDraft();
  React.useEffect(() => dispatch({ type: "schedule", schedule: WEEKLY }), [dispatch]);
  return draft.schedule?.repeat ? <ReviewWithLaunch {...props} /> : null;
}

function Where() {
  const l = useLocation();
  return <p data-testid="where">{l.pathname + l.search}</p>;
}

const posted = vi.fn();
beforeEach(() => {
  posted.mockReset();
  vi.spyOn(runs, "preflightRunWire").mockResolvedValue(CLEAR);
  vi.spyOn(runs, "previewRunPolicyWire").mockResolvedValue({ spec: {}, source: { kind: "inline" }, warnings: [], pending: [], repository_access: [] } as never);
  vi.spyOn(runs, "createRunWire").mockResolvedValue({ id: RUN_ID, state: "QUEUED", title: "x" } as never);
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    if (url === apiURL("/run-schedules")) {
      posted(init?.method, JSON.parse(String(init?.body)));
      return new Response(JSON.stringify({ id: "s-1", name: "Nightly", every: "weekly", weekday: 0, time: "09:00", time_zone: "Europe/London", next_at: "2026-11-29T09:00:00Z" }), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      });
    }
    return new Response(JSON.stringify({ error: "not found" }), { status: 404, headers: { "Content-Type": "application/json" } });
  });
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("Review makes a repeating schedule through the Name dialog", () => {
  it("says Create schedule, posts the schedule with the request inside and no start, and lands on Schedules", async () => {
    render(
      <MemoryRouter initialEntries={[`/runs/new?again=${RUN_ID}`]}>
        <OperatorProvider operator={false} principal="alice">
          <Routes>
            <Route path="/runs/new" element={<><NewRunRoute loaders={loaders} screens={{ ...NEW_RUN_SCREENS, review: ReviewRepeating }} /><Where /></>} />
            <Route path="/runs/schedules" element={<><p>schedules page</p><Where /></>} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    const create = await screen.findByRole("button", { name: RUN_SCHEDULE.CREATE });
    await waitFor(() => expect(create).toBeEnabled());
    await userEvent.click(create);
    const box = await screen.findByRole("dialog", { name: NR_LAUNCH.TITLE });
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "Nightly");
    await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
    await userEvent.click(within(box).getByRole("button", { name: new RegExp(`^${RUN_SCHEDULE.CREATE}$`) }));

    expect(await screen.findByText("schedules page")).toBeInTheDocument();
    expect(screen.getByTestId("where")).toHaveTextContent(/^\/runs\/schedules$/);
    expect(posted).toHaveBeenCalledTimes(1);
    const [method, body] = posted.mock.calls[0] as [string, Record<string, unknown>];
    expect(method).toBe("POST");
    expect(Object.keys(body).sort()).toEqual(["every", "name", "request", "time", "time_zone"]);
    expect(body).toMatchObject({ name: "Nightly", every: "weekly", time: "09:00", time_zone: "Europe/London" });
    expect(body.request).toMatchObject({ shape: "agent_task", workload: { agent: "claude-code", task: "nightly report" } });
    expect(body.request).not.toHaveProperty("start_at");
    expect(body.request).not.toHaveProperty("start_time_zone");
    expect(runs.createRunWire).not.toHaveBeenCalled();
  });
});
