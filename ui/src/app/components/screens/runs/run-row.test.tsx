/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F1 (#1197 L5, PR #1317 review) — Revive-from-row is the one row action
// that acts IN PLACE (POST /runs/{id}/revive) instead of navigating; this
// pins the click itself. runs-model.test.ts already pins that a lost `by=you`
// row gets action:"revive" in the first place.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { makeRun } from "../../../../test/factories";
import type { AgentRun } from "../../../lib/types";
import { RunRow } from "./run-row";

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
