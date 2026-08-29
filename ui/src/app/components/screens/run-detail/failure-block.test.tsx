/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M7(b) — "What happened / What to do". The regression this pins: a run that
// ended badly used to state its STATE and nothing else, leaving the reason in
// the audit trail for an operator to find through the API.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { AgentRun, AuditEvent, RunState } from "../../../lib/types";
import { runEndingFromAudit } from "../../../lib/api/audit";
import { RunFailureBlock } from "./failure-block";

const CREATED = "2026-08-28T10:00:00Z";

function run(state: RunState): AgentRun {
  return {
    id: "run_3b7f10c4-0000-0000-0000-000000000000",
    created_at: CREATED,
    updated_at: "2026-08-28T10:26:00Z",
    created_by: "alice",
    agent: "claude",
    repo: "acme/payments-api",
    state,
    spiffe_id: "spiffe://wardyn/run/3b7f10c4",
    runner_target: "runner://local",
    confinement_class: "CC2",
  } as AgentRun;
}

function ev(action: string, outcome: AuditEvent["outcome"], extra: Partial<AuditEvent> = {}): AuditEvent {
  return {
    id: `ev-${action}-${outcome}`,
    time: "2026-08-28T10:26:00Z",
    actor_type: "system",
    actor: "wardynd",
    action,
    outcome,
    ...extra,
  };
}

function renderBlock(state: RunState, audit: AuditEvent[], onGoAudit = vi.fn()) {
  render(<RunFailureBlock run={run(state)} audit={audit} onGoAudit={onGoAudit} />);
  return onGoAudit;
}

describe("runEndingFromAudit — the state picks the family, the audit picks the cause", () => {
  it("a run that ended fine gets no ending at all", () => {
    expect(runEndingFromAudit("COMPLETED", [ev("run.complete", "success")])).toBeUndefined();
    expect(runEndingFromAudit("RUNNING", [])).toBeUndefined();
    // A run someone stopped on purpose is not a failure — only the idle reaper
    // has something to explain.
    expect(runEndingFromAudit("STOPPED", [ev("run.complete", "success")])).toBeUndefined();
  });

  it("reads the FIRST failure event — the cause, not the fallout that followed it", () => {
    const trail = [
      ev("run.create", "success"),
      ev("run.build", "failure"),
      ev("run.selftest", "failure"),
    ];
    expect(runEndingFromAudit("FAILED", trail)?.kind).toBe("image");
  });

  it("run.dispatch/failure alone stays unknown — it covers too many causes to name one", () => {
    expect(runEndingFromAudit("FAILED", [ev("run.dispatch", "failure")])).toEqual({
      kind: "unknown",
      action: "",
    });
  });

  it("a KILLED run with no run.kill row still reports the kill — only the attribution is missing", () => {
    expect(runEndingFromAudit("KILLED", [])).toEqual({
      kind: "killed",
      action: "run.kill",
      outcome: undefined,
      actor: undefined,
      time: undefined,
    });
  });
});

describe("RunFailureBlock", () => {
  beforeEach(cleanup);

  it("renders nothing for a run that completed — the block is not page furniture", () => {
    const { container } = render(
      <RunFailureBlock run={run("COMPLETED")} audit={[ev("run.complete", "success")]} onGoAudit={vi.fn()} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("image: says nothing ran, and why there is no exit code", () => {
    renderBlock("FAILED", [ev("run.build", "failure")]);
    expect(screen.getByText("What happened")).toBeInTheDocument();
    expect(screen.getByText(/The sandbox image never pulled/)).toBeInTheDocument();
    expect(screen.getByText("What to do")).toBeInTheDocument();
    expect(screen.getByText(/Confirm the image tag in Base images still exists/)).toBeInTheDocument();
    expect(screen.getByTestId("run-failure-block")).toHaveAttribute("data-ending", "image");
  });

  it("selftest: says the run was refused before any task, and that there is nothing to clean up", () => {
    renderBlock("FAILED", [ev("run.selftest", "failure")]);
    expect(screen.getByText(/The image selftest failed/)).toBeInTheDocument();
    expect(screen.getByText(/there is nothing to clean up/)).toBeInTheDocument();
  });

  it("killed: names how far into the run it was killed, plus who and when on the audit row", () => {
    renderBlock("KILLED", [ev("run.kill", "success", { actor: "alice", actor_type: "human" })]);
    // created_at 10:00 -> run.kill 10:26.
    expect(screen.getByText(/An operator killed this run 26m 0s in\./)).toBeInTheDocument();
    expect(screen.getByText(/a killed run cannot resume/)).toBeInTheDocument();
    expect(screen.getByText(/run\.kill · success · alice/)).toBeInTheDocument();
  });

  it("auto_stop: framed as the policy working, not as a fault", () => {
    renderBlock("STOPPED", [ev("run.autostop", "success", { actor: "wardyn/lifecycle-reaper" })]);
    expect(screen.getByText(/This is the policy working, not a fault\./)).toBeInTheDocument();
    expect(screen.getByText(/Raise auto_stop_after_sec in the policy/)).toBeInTheDocument();
  });

  it("unknown: the audit link and nothing else — no invented cause, no generic advice", () => {
    renderBlock("FAILED", [ev("run.dispatch", "failure")]);
    expect(screen.getByTestId("run-failure-block")).toHaveAttribute("data-ending", "unknown");
    expect(screen.queryByText("What happened")).not.toBeInTheDocument();
    expect(screen.queryByText("What to do")).not.toBeInTheDocument();
    // Still actionable: the trail is one click away.
    expect(screen.getByRole("button", { name: /Open audit trail/ })).toBeInTheDocument();
  });

  it("Open audit trail hands off to the Audit tab", async () => {
    const onGoAudit = renderBlock("FAILED", [ev("run.build", "failure")]);
    await userEvent.click(screen.getByRole("button", { name: /Open audit trail/ }));
    expect(onGoAudit).toHaveBeenCalledTimes(1);
  });
});
