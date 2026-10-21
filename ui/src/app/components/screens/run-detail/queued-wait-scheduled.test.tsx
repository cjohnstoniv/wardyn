/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's two scheduled heroes (FEAT-2 boards F2SCHED-2 and -3): a run that waits for its time, and a due
// run that waits for its owner's sign-in.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { makeRun } from "../../../../test/factories";
import { WithDoor } from "../../../../test/door-harness";
import { runs } from "../../../lib/api/runs";
import { whenText } from "../../../lib/schedule";
import { MODEL_PROVIDERS, providerStatus } from "../../../lib/test-fixtures";
import type { RunDetail, RunQueue } from "../../../lib/types";
import { MODEL_ACCESS_BANNER } from "../../wardyn/model-access-copy";
import { CONNECTIONS } from "../../wardyn/copy/door";
import { SCHEDULED_RUN } from "../../wardyn/copy/scheduled-runs";
import { TerminalPane } from "./terminal-notice";

vi.mock("../../attach-terminal", () => ({ AttachTerminal: () => null }));
vi.mock("../settings/harness-login-pane", () => ({ HarnessLoginPane: () => <div data-testid="fake-pane" /> }));
vi.mock("../../../lib/api/runner-pools", () => ({ runnerPools: { list: () => Promise.resolve({ pools: [] }) } }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const START = new Date(Date.now() + 12 * 3600_000).toISOString();
const DEADLINE = new Date(Date.now() + 7 * 86_400_000).toISOString();
const queue = (reason: string, over: Partial<RunQueue> = {}): RunQueue => ({
  pool_id: "pool-1", queued_at: new Date(Date.now() - 3600_000).toISOString(), deadline_at: DEADLINE, position: 0, reason,
  start_at: START, time_zone: "UTC", ...over,
});
const scheduled = (reason: string, over: Partial<RunDetail> = {}, q: Partial<RunQueue> = {}) =>
  makeRun({ id: "run-1", state: "QUEUED", created_by: "bob", title: "Nightly dependency bump", queue: queue(reason, q), ...over }) as RunDetail;

function show(run: RunDetail, status = providerStatus([{ provider: MODEL_PROVIDERS.bedrock }])) {
  window.history.pushState({}, "", "/runs/run-1");
  render(
    <WithDoor path="/runs/run-1" principal="bob" operator={false} status={status}>
      <TerminalPane run={run} terminal={false} recording={null} recState="idle" recordingDisabled={false} onGoRecording={vi.fn()} execMode={false} />
    </WithDoor>,
  );
}

beforeEach(() => vi.spyOn(runs, "killRun").mockResolvedValue(undefined));
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  window.history.pushState({}, "", "/");
});

describe("a scheduled run's page", () => {
  it("says Scheduled, when it starts and in how long, and that nothing is held until then", () => {
    show(scheduled("scheduled"));
    expect(screen.getByRole("heading", { name: SCHEDULED_RUN.WORD })).toBeInTheDocument();
    expect(screen.getByText(SCHEDULED_RUN.PAGE_STARTS(whenText(START, "UTC"), "12h"))).toBeInTheDocument();
    expect(screen.getByText(SCHEDULED_RUN.PAGE_NOTE)).toBeInTheDocument();
    expect(screen.queryByTestId("run-queued-wait-line")).not.toBeInTheDocument();
  });

  it("reads the start in the zone it was given in", () => {
    show(scheduled("scheduled", {}, { time_zone: "America/New_York" }));
    expect(screen.getByText(new RegExp(`Starts .* \\(America/New_York\\) · in 12h`))).toBeInTheDocument();
  });

  it("Cancel run asks first, then kills the run", async () => {
    show(scheduled("scheduled"));
    await userEvent.click(screen.getByRole("button", { name: SCHEDULED_RUN.CANCEL_RUN }));
    expect(runs.killRun).not.toHaveBeenCalled();
    await userEvent.click(await screen.findByRole("button", { name: "Kill run" }));
    await waitFor(() => expect(runs.killRun).toHaveBeenCalledWith("run-1"));
  });
});

describe("a due run waiting for its owner's sign-in", () => {
  const signIn = () => scheduled("owner_sign_in", { model_provider_id: MODEL_PROVIDERS.bedrock.id, model_provider_name: MODEL_PROVIDERS.bedrock.name });

  it("says whose sign-in, when it was due and the day it fails without starting", () => {
    show(signIn());
    expect(screen.getByRole("heading", { name: SCHEDULED_RUN.SIGN_IN_TITLE })).toBeInTheDocument();
    expect(screen.getByText(CONNECTIONS.C6_LINE(MODEL_PROVIDERS.bedrock.name))).toBeInTheDocument();
    expect(screen.getByText(SCHEDULED_RUN.SIGN_IN_BODY(whenText(START, "UTC"), whenText(DEADLINE, "UTC")))).toBeInTheDocument();
  });

  it("offers its owner Sign in again, which opens the provider's door, and Cancel run", async () => {
    show(signIn());
    await userEvent.click(screen.getByRole("button", { name: CONNECTIONS.SIGN_IN_AGAIN }));
    expect(await screen.findByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeInTheDocument();
    cleanup();
    show(signIn());
    expect(screen.getByRole("button", { name: SCHEDULED_RUN.CANCEL_RUN })).toBeInTheDocument();
  });

  it("offers someone else no sign-in: it lands in the signer's own namespace", () => {
    show({ ...signIn(), created_by: "alice" } as RunDetail);
    expect(screen.queryByRole("button", { name: CONNECTIONS.SIGN_IN_AGAIN })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: SCHEDULED_RUN.CANCEL_RUN })).toBeInTheDocument();
  });
});

describe("any other queued run", () => {
  it("keeps the pool hero", () => {
    show(scheduled("runner_pool_at_capacity", {}, { start_at: undefined, time_zone: undefined }));
    expect(screen.getByTestId("run-queued-wait")).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: SCHEDULED_RUN.WORD })).not.toBeInTheDocument();
  });
});
