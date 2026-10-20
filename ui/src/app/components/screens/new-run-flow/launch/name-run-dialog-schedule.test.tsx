/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Name dialog of a run that starts later: the Starts line, the held Launch, the start in the create body, and
// the refusals a start can draw. The rest of the dialog is name-run-dialog.test.tsx's.

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { HttpError } from "../../../../lib/api/core";
import { runs } from "../../../../lib/api/runs";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import { whenText, type StartChoice } from "../../../../lib/schedule";
import { SCHEDULE_REFUSAL } from "../../../../lib/schedule-refusals";
import type { PreflightResult } from "../../../../lib/types/preflight";
import { NR_LAUNCH } from "../../../wardyn/copy/new-run/launch";
import { NR_RUN } from "../../../wardyn/copy/new-run/run";
import { OperatorProvider } from "../../../wardyn/operator-context";
import type { RunIntent } from "../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../new-run-route";
import { NameRunDialog } from "./name-run-dialog";

const shell = vi.hoisted(() => ({ status: {} as Record<string, unknown> }));
vi.mock("../../../wardyn/model-access-context", () => ({
  useModelAccessDoor: () => ({ openDoor: vi.fn(), refresh: vi.fn() }),
  useShellSetupStatus: () => ({ status: shell.status }),
}));

const AUTONOMOUS: RunIntent = { shape: "agent_task", experience: "background", workload: { kind: "agent_task", agent: "claude-code", task: "x" } };
const LIFETIME = { default: { value: 3600 }, cap: { value: 7200 } };
const CLEAR = { ...SCENARIO_PREFLIGHT.member, limits: { lifetime_sec: LIFETIME } } as unknown as PreflightResult;
const tomorrow = (): StartChoice => ({ date: new Date(Date.now() + 86_400_000).toISOString().slice(0, 10), time: "12:00", zone: "UTC" });
const create = vi.fn();

function Seeded({ intent, schedule, children }: { intent: RunIntent; schedule?: StartChoice; children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const draft = useNewRunDraft();
  React.useEffect(() => {
    dispatch({ type: "seed", origin: { kind: "scratch" }, intent });
    dispatch({ type: "schedule", schedule });
  }, [dispatch, intent, schedule]);
  return draft.intent === intent ? <>{children}</> : null;
}

function open(schedule: StartChoice | undefined, intent: RunIntent = AUTONOMOUS) {
  render(
    <MemoryRouter>
      <OperatorProvider operator={false} principal="alice">
        <NewRunDraftProvider owner="alice">
          <Seeded intent={intent} schedule={schedule}>
            <NameRunDialog open onOpenChange={vi.fn()} onCreated={vi.fn()} />
          </Seeded>
        </NewRunDraftProvider>
      </OperatorProvider>
    </MemoryRouter>,
  );
}
const launchButton = () => screen.getByRole("button", { name: new RegExp(`^${NR_LAUNCH.LAUNCH}$`) });
const named = async (box: HTMLElement) => {
  await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "Nightly");
  await waitFor(() => expect(within(box).getByText(NR_LAUNCH.CHECKED_JUST_NOW)).toBeInTheDocument());
};

beforeEach(() => {
  shell.status = { features: { scheduled_runs: true } };
  create.mockReset().mockResolvedValue({ id: "run-1", state: "QUEUED" });
  vi.spyOn(runs, "preflightRunWire").mockResolvedValue(CLEAR);
  vi.spyOn(runs, "previewRunPolicyWire").mockResolvedValue({} as never);
  vi.spyOn(runs, "createRunWire").mockImplementation(create);
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("Name this run: a start later", () => {
  it("says when it starts and creates with start_at and start_time_zone, which the check never carried", async () => {
    const schedule = tomorrow();
    open(schedule);
    const box = await screen.findByRole("dialog", { name: NR_LAUNCH.TITLE });
    await named(box);
    expect(within(box).getByText(NR_RUN.STARTS(whenText(Date.parse(`${schedule.date}T12:00:00Z`), "UTC")))).toBeInTheDocument();
    expect(vi.mocked(runs.preflightRunWire).mock.calls[0][0]).not.toHaveProperty("start_at");
    await userEvent.click(launchButton());
    await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    expect(create.mock.calls[0][0]).toMatchObject({ title: "Nightly", start_at: `${schedule.date}T12:00`, start_time_zone: "UTC" });
  });

  it("holds Launch on a start in the past, with the board's sentence, and sends nothing", async () => {
    open({ date: "2020-01-01", time: "12:00", zone: "UTC" });
    const box = await screen.findByRole("dialog", { name: NR_LAUNCH.TITLE });
    await named(box);
    expect(within(box).getByRole("alert")).toHaveTextContent(NR_RUN.START_PAST);
    expect(launchButton()).toBeDisabled();
    expect(launchButton()).toHaveAttribute("aria-describedby", "name-run-start");
    expect(create).not.toHaveBeenCalled();
  });

  it("starts now, with no line and no start in the body, when the switch is off or the run is Interactive", async () => {
    shell.status = {};
    open(tomorrow());
    let box = await screen.findByRole("dialog", { name: NR_LAUNCH.TITLE });
    await named(box);
    expect(within(box).queryByText(/^Starts /)).not.toBeInTheDocument();
    await userEvent.click(launchButton());
    await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    expect(create.mock.calls[0][0]).not.toHaveProperty("start_at");
    cleanup();

    shell.status = { features: { scheduled_runs: true } };
    create.mockClear();
    open(tomorrow(), { shape: "agent_task", experience: "interactive" });
    box = await screen.findByRole("dialog", { name: NR_LAUNCH.TITLE });
    await named(box);
    expect(within(box).queryByText(/^Starts /)).not.toBeInTheDocument();
  });

  it("shows a start refusal as a sentence and never as its code", async () => {
    create.mockRejectedValue(new HttpError(422, "start_at_past", "start_at_past"));
    open(tomorrow());
    const box = await screen.findByRole("dialog", { name: NR_LAUNCH.TITLE });
    await named(box);
    await userEvent.click(launchButton());
    expect(await within(box).findByText(NR_RUN.START_PAST, { selector: "#name-run-sentence" })).toBeInTheDocument();
    expect(within(box).queryByText("start_at_past")).not.toBeInTheDocument();
    create.mockRejectedValue(new HttpError(422, SCHEDULE_REFUSAL.UNAVAILABLE(), "scheduled_runs_unavailable"));
    await userEvent.click(launchButton());
    expect(await within(box).findByText(SCHEDULE_REFUSAL.UNAVAILABLE())).toBeInTheDocument();
  });
});
