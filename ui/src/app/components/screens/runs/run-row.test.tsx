/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F1 (#1197 L5, PR #1317 review) — Revive-from-row is the one row action
// that acts IN PLACE (POST /runs/{id}/revive) instead of navigating; this
// pins the click itself. runs-model.test.ts already pins that a lost `by=you`
// row gets action:"revive" in the first place.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { makeRun } from "../../../../test/factories";
import type { AgentRun } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { RUNS_ROW_ACTION, RUNS_ROW_WORD } from "../../wardyn/copy/runs-landing";
import { RunRow } from "./run-row";

const signInMock = vi.fn();
vi.mock("../../../lib/api/run-sign-in", () => ({ runSignIn: { get: (...a: unknown[]) => signInMock(...a) } }));
const reviveRunMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: { reviveRun: (...a: unknown[]) => reviveRunMock(...a) },
}));
const toastErrorMock = vi.fn();
const toastInfoMock = vi.fn();
vi.mock("sonner", () => ({
  toast: { error: (...a: unknown[]) => toastErrorMock(...a), info: (...a: unknown[]) => toastInfoMock(...a) },
}));

function renderRow(run: AgentRun) {
  return render(
    <MemoryRouter initialEntries={["/runs"]}>
      <RunRow run={run} />
    </MemoryRouter>,
  );
}

beforeEach(() => vi.clearAllMocks());

describe("RunRow — Revive (F1)", () => {
  it("a lost row I own offers Revive, and clicking it calls reviveRun — never a navigation", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    reviveRunMock.mockResolvedValue({ run_id: "run-1", denied_added: [], proxy_release: "r1" });
    const run = makeRun({ attention: { kind: "lost", by: "you", pending: 0 } });
    renderRow(run);
    const button = screen.getByRole("button", { name: "Revive" });
    await user.click(button);
    await waitFor(() => expect(reviveRunMock).toHaveBeenCalledWith(run.id));
  });

  it("shows Reviving… while the call is in flight, then Revive again on failure", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    let reject!: (e: Error) => void;
    reviveRunMock.mockReturnValue(new Promise((_res, rej) => { reject = rej; }));
    const run = makeRun({ attention: { kind: "lost", by: "you", pending: 0 } });
    renderRow(run);
    await user.click(screen.getByRole("button", { name: "Revive" }));
    expect(await screen.findByRole("button", { name: "Reviving… (about a minute)" })).toBeInTheDocument();
    reject(new Error("boom"));
    expect(await screen.findByRole("button", { name: "Revive" })).toBeInTheDocument();
    expect(toastErrorMock).toHaveBeenCalled();
  });

  it("by=owner (not you): no action button at all", () => {
    const run = makeRun({ attention: { kind: "lost", by: "owner", pending: 0 } });
    renderRow(run);
    expect(screen.queryByRole("button", { name: "Revive" })).not.toBeInTheDocument();
  });

  // F12 (PR #1317 round-2 review): the run page's own Revive already reports
  // newly-blocked hosts — the row's Revive is the identical call and owed
  // the same toast, not a silent success.
  it("F12: reports newly-blocked hosts on a successful revive", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    reviveRunMock.mockResolvedValue({ run_id: "run-1", denied_added: ["evil.example"], proxy_release: "r1" });
    const run = makeRun({ attention: { kind: "lost", by: "you", pending: 0 } });
    renderRow(run);
    await user.click(screen.getByRole("button", { name: "Revive" }));
    await waitFor(() => expect(toastInfoMock).toHaveBeenCalledWith("Policy updated at revive: 1 host now blocked."));
  });

  // F18 (PR #1317 round-2 review): no k8s revive in 0.8 — the run page's own
  // lifetime banner already treats a k8s lost run as not revivable, so the
  // row must not dangle a button the run page itself won't honour.
  it("F18: a k8s lost row I own offers no Revive at all", () => {
    const run = makeRun({ attention: { kind: "lost", by: "you", pending: 0 }, runner_target: "k8s" });
    renderRow(run);
    expect(screen.queryByRole("button", { name: "Revive" })).not.toBeInTheDocument();
  });
});

// M2: the Runs row reads a sign-in for the viewer's own RUNNING sign-in runs only.
describe("RunRow — a waiting sign-in (M2)", () => {
  const signInRun = (o: Partial<AgentRun> = {}) =>
    makeRun({ task: "harness login", agent: "aws-sso", state: "RUNNING", created_by: "me", ...o });
  const renderMine = (run: AgentRun, principal = "me") =>
    render(
      <OperatorProvider operator={false} principal={principal}>
        <MemoryRouter initialEntries={["/runs"]}>
          <RunRow run={run} />
        </MemoryRouter>
      </OperatorProvider>,
    );

  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());
  const flush = () => act(async () => void (await vi.advanceTimersByTimeAsync(0)));

  it("answers waiting: the amber word and the Sign in action, in the Running section", async () => {
    signInMock.mockResolvedValue({
      state: "waiting",
      verification_url: "https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH",
      user_code: "ABCD-EFGH",
    });
    renderMine(signInRun());
    await flush();
    expect(screen.getByText(RUNS_ROW_WORD.WAITING_SIGN_IN)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: RUNS_ROW_ACTION.SIGN_IN })).toBeInTheDocument();
    expect(screen.getByTestId("run-row")).not.toHaveAttribute("data-attention");
  });

  it("answers not_waiting: the row is the plain Running row", async () => {
    signInMock.mockResolvedValue({ state: "not_waiting" });
    renderMine(signInRun({ created_at: new Date(Date.now() - 3_600_000).toISOString() }));
    await flush();
    expect(screen.getByText(RUNS_ROW_WORD.RUNNING)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: RUNS_ROW_ACTION.SIGN_IN })).not.toBeInTheDocument();
  });

  it("a failed read draws nothing on the row", async () => {
    signInMock.mockRejectedValue(new Error("503"));
    renderMine(signInRun());
    await flush();
    expect(screen.getByText(RUNS_ROW_WORD.RUNNING)).toBeInTheDocument();
  });

  it.each([
    ["someone else's sign-in run", signInRun({ created_by: "someone-else" }), "me"],
    ["an unresolved identity", signInRun(), ""],
    ["a run that is not a sign-in", makeRun({ created_by: "me" }), "me"],
    ["a sign-in run that is not running", signInRun({ state: "COMPLETED" }), "me"],
  ])("reads nothing for %s", async (_n, run, principal) => {
    renderMine(run, principal);
    await flush();
    expect(signInMock).not.toHaveBeenCalled();
  });
});
