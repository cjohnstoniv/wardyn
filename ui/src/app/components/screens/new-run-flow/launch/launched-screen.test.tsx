/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { runnerPools } from "../../../../lib/api/runner-pools";
import { runs } from "../../../../lib/api/runs";
import { shortTime } from "../../../../lib/format";
import { whenText } from "../../../../lib/schedule";
import { SCHEDULED_RUN } from "../../../wardyn/copy/scheduled-runs";
import type { RunnerPoolList } from "../../../../lib/types/runner-pools";
import { NR_LAUNCH, NR_LAUNCH_STALE } from "../../../wardyn/copy/new-run/launch";
import { RUN_NOT_FOUND } from "../../../wardyn/copy/refusal";
import { OperatorProvider } from "../../../wardyn/operator-context";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft, type LaunchedState } from "../new-run-route";
import { LaunchedScreen } from "./launched-screen";
import type { RunQueue } from "../../../../lib/types/runs";
import type { LaunchedRun } from "./launched-model";

const ID = "b2222222-2222-4222-8222-222222222222";
const HOUR = 3_600_000;
const queue = (over: Partial<RunQueue> = {}): RunQueue => ({
  pool_id: "pool-ci", queued_at: new Date().toISOString(), deadline_at: new Date(Date.now() + 24 * HOUR).toISOString(), position: 1,
  reason: "runner_pool_at_capacity", ...over,
});
const run = (over: Partial<LaunchedRun> = {}): LaunchedRun => ({ id: ID, state: "PENDING", title: "nightly build", ...over }) as LaunchedRun;
const pools = { pools: [{ id: "pool-ci", name: "CI jobs", slots: { used: 40, max: 40, waiting: 1, verdict: "waits" } }] } as unknown as RunnerPoolList;

const getRun = vi.fn();
const killRun = vi.fn();
const go = vi.fn();
function Where() {
  const l = useLocation();
  return <p data-testid="where">{l.pathname + l.search}</p>;
}

function RunPage() {
  const carried = (useLocation().state as { launchWarnings?: string[] } | null)?.launchWarnings ?? [];
  return <><p>run page</p><p data-testid="carried">{JSON.stringify(carried)}</p></>;
}

function Typed() {
  const dispatch = useNewRunDispatch();
  const { launch } = useNewRunDraft();
  React.useEffect(() => dispatch({ type: "launchFields", patch: { title: "typed before" } }), [dispatch]);
  return <p data-testid="typed">{launch.title}</p>;
}

function open(state?: LaunchedState) {
  render(
    <MemoryRouter initialEntries={[{ pathname: "/runs/new", search: `?launched=${ID}`, state }]}>
      <OperatorProvider operator={false} principal="alice">
        <NewRunDraftProvider owner="alice">
          <Typed />
          <Routes>
            <Route path="/runs/new" element={<LaunchedScreen location={{ screen: "launched", runId: ID }} go={go} startFromScratch={() => {}} />} />
            <Route path="/runs/:id" element={<RunPage />} />
          </Routes>
          <Where />
        </NewRunDraftProvider>
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getRun.mockReset().mockResolvedValue(run());
  killRun.mockReset().mockResolvedValue(undefined);
  go.mockReset();
  vi.spyOn(runs, "getRun").mockImplementation(getRun);
  vi.spyOn(runs, "killRun").mockImplementation(killRun);
  vi.spyOn(runnerPools, "list").mockResolvedValue(pools);
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe("Launched: while the run loads (UXR-48)", () => {
  it("draws a placeholder for the badge and the title, then the run", async () => {
    let settle!: (r: unknown) => void;
    getRun.mockReturnValue(new Promise((r) => (settle = r)));
    open();
    const loading = await screen.findByTestId("launched-loading");
    expect(loading).toHaveAttribute("aria-busy", "true");
    expect(screen.queryByRole("heading", { level: 1 })).toBeNull();
    await act(async () => settle(run()));
    expect(await screen.findByRole("heading", { level: 1, name: "nightly build" })).toBeInTheDocument();
    expect(screen.queryByTestId("launched-loading")).toBeNull();
  });
});

describe("Launched: Waiting (U13)", () => {
  it.each([
    [0, "1st in line"],
    [1, "2nd in line"],
    [3, "4th in line"],
  ])("a run with %i waiting ahead reads %s", async (position, words) => {
    getRun.mockResolvedValue(run({ state: "QUEUED", queue: queue({ position }) }));
    open();
    expect(await screen.findByText(NR_LAUNCH.BADGE_WAITING)).toBeInTheDocument();
    expect(await screen.findByText(words)).toBeInTheDocument();
    expect(screen.queryByText(`${position} in line`)).not.toBeInTheDocument();
  });

  it("shows since, when it gives up from deadline_at, and why it waits in the pool's own words", async () => {
    const queuedAt = new Date(Date.now() - 5 * 60_000).toISOString();
    getRun.mockResolvedValue(run({
      state: "QUEUED",
      queue: queue({ queued_at: queuedAt, deadline_at: new Date(Date.now() + 100 * 60_000).toISOString() }),
    }));
    open();
    expect(await screen.findByText(NR_LAUNCH.WAITING_SINCE)).toBeInTheDocument();
    expect(screen.getByText(shortTime(queuedAt))).toBeInTheDocument();
    expect(screen.getByText(NR_LAUNCH.GIVES_UP)).toBeInTheDocument();
    expect(screen.getByText("In 2 hours")).toBeInTheDocument();
    expect(await screen.findByText(NR_LAUNCH.WAITING_BODY("CI jobs", 40))).toBeInTheDocument();
    expect(screen.queryByRole("progressbar")).not.toBeInTheDocument();
  });

  it("Cancel run goes through the kill dialog and kills only the run it names", async () => {
    getRun.mockResolvedValue(run({ state: "QUEUED", queue: queue() }));
    open();
    await userEvent.click(await screen.findByRole("button", { name: NR_LAUNCH.CANCEL_RUN }));
    const confirm = await screen.findByRole("alertdialog", { name: "End nightly build?" });
    expect(killRun).not.toHaveBeenCalled();
    await userEvent.click(within(confirm).getByRole("button", { name: "End run" }));
    await waitFor(() => expect(killRun).toHaveBeenCalledWith(ID));
  });
});

describe("Launched: a scheduled run", () => {
  const START = new Date(Date.now() + 12 * HOUR).toISOString();

  it("says when it starts and what is checked then, with no place in line, and can still be cancelled", async () => {
    getRun.mockResolvedValue(run({ state: "QUEUED", queue: queue({ reason: "scheduled", start_at: START, time_zone: "UTC", position: 0 }) }));
    open();
    const body = await screen.findByTestId("launched-scheduled");
    expect(body).toHaveTextContent(SCHEDULED_RUN.PAGE_STARTS(whenText(START, "UTC"), "12h"));
    expect(body).toHaveTextContent(SCHEDULED_RUN.PAGE_NOTE);
    expect(screen.queryByText(NR_LAUNCH.POSITION)).not.toBeInTheDocument();
    expect(screen.queryByText(NR_LAUNCH.WAITING_BODY("CI jobs", 40))).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: NR_LAUNCH.CANCEL_RUN })).toBeInTheDocument();
  });
});

describe("Launched: Starting", () => {
  it("shows an indeterminate bar and no queue, from the run's own title", async () => {
    open();
    expect(await screen.findByText(NR_LAUNCH.BADGE_STARTING)).toBeInTheDocument();
    expect(screen.getByRole("heading", { level: 1, name: "nightly build" })).toHaveFocus();
    expect(screen.getByText(NR_LAUNCH.STARTING_BODY)).toBeInTheDocument();
    const bar = screen.getByRole("progressbar");
    expect(bar).not.toHaveAttribute("aria-valuenow");
    expect(screen.queryByText(NR_LAUNCH.POSITION)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: NR_LAUNCH.CANCEL_RUN })).not.toBeInTheDocument();
  });

  it("View run opens the run with the create's warnings; Start another run goes to a blank Start", async () => {
    const created = run({ warnings: ["another run uses this folder"] }) as LaunchedState["created"];
    open({ created, owner: "alice" });
    const link = await screen.findByRole("link", { name: NR_LAUNCH.VIEW_RUN });
    expect(link).toHaveAttribute("href", `/runs/${ID}`);
    await userEvent.click(screen.getByRole("button", { name: NR_LAUNCH.START_ANOTHER }));
    expect(go).toHaveBeenCalledWith({ screen: "start" });
    await userEvent.click(link);
    expect(screen.getByText("run page")).toBeInTheDocument();
  });
});

describe("Launched: a run its runners hold back", () => {
  const named = "Every runner in CI jobs is at a limit. For example, mac-1: Runs at once: 1 running of 1. This run starts when one has room.";
  const held = (over: Partial<RunQueue> = {}) => queue({ reason: "runner_pool_runners_at_capacity", ...over });

  it("says the limit the server named, and the run page carries no second copy of it", async () => {
    const created = run({ state: "QUEUED", queue: held({ limit_sentence: named }), warnings: ["another run uses this folder", named] }) as LaunchedState["created"];
    getRun.mockResolvedValue(created);
    open({ created, owner: "alice" });
    expect(await screen.findByText(named)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("link", { name: NR_LAUNCH.VIEW_RUN }));
    expect(screen.getByTestId("carried")).toHaveTextContent(JSON.stringify(["another run uses this folder"]));
  });

  it("follows the wait when it has changed since the create: the read's words win and the stale copy is not carried", async () => {
    const created = run({ state: "QUEUED", queue: held({ limit_sentence: named }), warnings: [named] }) as LaunchedState["created"];
    getRun.mockResolvedValue(run({ state: "QUEUED", queue: queue({ reason: "runner_pool_no_runner_free" }) }));
    open({ created, owner: "alice" });
    expect(await screen.findByText("None of your runners in CI jobs is online. This run starts when one is.")).toBeInTheDocument();
    expect(screen.queryByText(named)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("link", { name: NR_LAUNCH.VIEW_RUN }));
    expect(screen.getByTestId("carried")).toHaveTextContent("[]");
  });
});

describe("Launched: reload and identity", () => {
  it("reads the run by its id when there is no router state, and creates nothing", async () => {
    const create = vi.spyOn(runs, "createRunWire");
    open();
    expect(await screen.findByText(NR_LAUNCH.BADGE_STARTING)).toBeInTheDocument();
    expect(getRun).toHaveBeenCalledWith(ID);
    expect(create).not.toHaveBeenCalled();
  });

  it("never shows another person's created run from router state while the read is out", async () => {
    getRun.mockReturnValue(new Promise(() => {}));
    open({ created: run({ title: "bob's secret run" }) as LaunchedState["created"], owner: "bob" });
    await act(async () => {});
    expect(screen.queryByText("bob's secret run")).not.toBeInTheDocument();
  });

  it("shows the created run at once for the person who made it", async () => {
    getRun.mockReturnValue(new Promise(() => {}));
    open({ created: run({ title: "mine" }) as LaunchedState["created"], owner: "alice" });
    expect(await screen.findByRole("heading", { level: 1, name: "mine" })).toBeInTheDocument();
  });

  it("is the end of the draft: what was typed does not come back", async () => {
    open();
    await screen.findByText(NR_LAUNCH.BADGE_STARTING);
    await waitFor(() => expect(screen.getByTestId("typed")).toHaveTextContent(/^$/));
  });

  it("moves focus to the heading when Cancel run takes the run out of Waiting", async () => {
    getRun.mockResolvedValueOnce(run({ state: "QUEUED", queue: queue() })).mockResolvedValue(run({ state: "KILLED" }));
    open();
    await userEvent.click(await screen.findByRole("button", { name: NR_LAUNCH.CANCEL_RUN }));
    const confirm = await screen.findByRole("alertdialog", { name: "End nightly build?" });
    await userEvent.click(within(confirm).getByRole("button", { name: "End run" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: NR_LAUNCH.CANCEL_RUN })).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getByRole("heading", { level: 1, name: "nightly build" })).toHaveFocus());
  });

  it("keeps reading while the run is starting or waiting, then stops", async () => {
    vi.useFakeTimers();
    getRun.mockResolvedValueOnce(run({ state: "QUEUED", queue: queue() })).mockResolvedValueOnce(run({ state: "RUNNING" }));
    open();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10);
    });
    expect(getRun).toHaveBeenCalledTimes(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4100);
    });
    expect(getRun).toHaveBeenCalledTimes(2);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(12_000);
    });
    expect(getRun).toHaveBeenCalledTimes(2);
  });

  it("says the run was not found, with no retry, and a way back to Runs", async () => {
    getRun.mockResolvedValue(undefined);
    open();
    expect(await screen.findByRole("heading", { level: 1, name: RUN_NOT_FOUND.TITLE })).toBeInTheDocument();
    expect(screen.queryByText(NR_LAUNCH.BADGE_STARTING)).not.toBeInTheDocument();
    expect(screen.queryByText(/isn't answering/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /try again|retry/i })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: RUN_NOT_FOUND.CTA })).toHaveAttribute("href", "/runs");
  });

  it("keeps the run on screen and says it cannot refresh when a later read fails", async () => {
    vi.useFakeTimers();
    getRun.mockResolvedValueOnce(run({ state: "QUEUED", queue: queue() })).mockRejectedValue(new Error("Failed to fetch"));
    open();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10);
    });
    expect(screen.queryByTestId("launched-stale")).not.toBeInTheDocument();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4100);
    });
    expect(screen.getByTestId("launched-stale")).toHaveTextContent(NR_LAUNCH_STALE);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("nightly build");
  });
});
