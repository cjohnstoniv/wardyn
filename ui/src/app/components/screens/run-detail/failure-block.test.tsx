/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M7(b) — "What happened / What to do": a run that ended badly must state
// more than its state — the reason must not be left in the audit trail alone
// for an operator to find through the API.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { AgentRun, AuditEvent, RunState } from "../../../lib/types";
import { makeRun } from "../../../../test/factories";
import { runEndingFromAudit } from "../../../lib/api/audit";
import { RunFailureBlock } from "./failure-block";
import { LOCAL_RUN, runnerLabel } from "../../wardyn/copy/runners";
import type { HeldCredential, HeldCredentials } from "../../../lib/held-credentials";

const ENV_SECRET_LINE = "Wardyn can't revoke a secret this run received as an environment variable — rotate it where it was issued.";
const UNREADABLE_LINE = "Wardyn couldn't read which credentials this run held.";

// The "killed: names how far into the run..." test below asserts an exact
// "26m 0s" elapsed (ending.time or run.updated_at, minus run.created_at —
// failure-block.tsx:185-186). aheadByHours calls Date.now() fresh on every
// call, and CREATED (a module-level constant) vs. the ev()/run() defaults
// (evaluated per-test, much later) could drift by however long the suite
// takes to reach this file's tests — enough to round the seconds differently.
// A single frozen anchor keeps the two exactly 26 minutes apart regardless of
// when the test actually runs.
const NOW_MS = Date.now();
const CREATED = new Date(NOW_MS - 26 * 60 * 1000).toISOString();
const EVENT_TIME = new Date(NOW_MS).toISOString();

function run(state: RunState): AgentRun {
  return makeRun({
    id: "run_3b7f10c4-0000-0000-0000-000000000000",
    created_at: CREATED,
    updated_at: EVENT_TIME,
    created_by: "alice",
    agent: "claude",
    repo: "acme/payments-api",
    state,
    spiffe_id: "spiffe://wardyn/run/3b7f10c4",
    runner_target: "runner://local",
    confinement_class: "CC2",
  });
}

function ev(action: string, outcome: AuditEvent["outcome"], extra: Partial<AuditEvent> = {}): AuditEvent {
  return {
    id: `ev-${action}-${outcome}`,
    time: EVENT_TIME,
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

  it("a KILLED run with no run.kill row still reports the kill — only the attribution is missing, and the evidence is unknown", () => {
    expect(runEndingFromAudit("KILLED", [])).toEqual({
      kind: "killed",
      action: "run.kill",
      outcome: undefined,
      actor: undefined,
      time: undefined,
      evidence: "unknown",
    });
  });

  // #1487: the LATEST kill row decides, by its own timestamp — not by where a
  // concatenated fetch happened to put it.
  it("evidence follows the latest run.kill row: failure then success is confirmed, success then failure is partial", () => {
    const early = new Date(NOW_MS - 120_000).toISOString();
    const late = new Date(NOW_MS - 60_000).toISOString();
    const fail = ev("run.kill", "failure", { time: early });
    const ok = ev("run.kill", "success", { time: late });
    expect(runEndingFromAudit("KILLED", [fail, ok])).toMatchObject({ evidence: "confirmed", outcome: "success" });
    expect(runEndingFromAudit("KILLED", [ok, fail])).toMatchObject({ evidence: "confirmed", outcome: "success" });
    const okFirst = ev("run.kill", "success", { time: early });
    const failLast = ev("run.kill", "failure", { time: late });
    expect(runEndingFromAudit("KILLED", [okFirst, failLast])).toMatchObject({ evidence: "partial", outcome: "failure" });
    expect(runEndingFromAudit("KILLED", [failLast, okFirst])).toMatchObject({ evidence: "partial", outcome: "failure" });
  });

  it("only a KILLED ending carries evidence", () => {
    expect(runEndingFromAudit("FAILED", [ev("run.build", "failure")])).not.toHaveProperty("evidence");
  });

  // An interactive BYOI run's selftest is warn-only (runs_dispatch_byoi.go's
  // byoiSelftest(…, false)): it records run.selftest/failure and the run carries
  // on. Treating that row as the cause would tell an operator whose run failed an
  // hour later that it was "refused before any task ran" — a false diagnosis of a
  // run that ran.
  it("a warn-only selftest row is not a cause — fail_closed:false disqualifies it", () => {
    const warned = ev("run.selftest", "failure", { data: { fail_closed: false } });
    expect(runEndingFromAudit("FAILED", [warned])).toEqual({ kind: "unknown", action: "" });

    // fail-closed, and an older trail with no flag at all, both still count.
    expect(runEndingFromAudit("FAILED", [ev("run.selftest", "failure", { data: { fail_closed: true } })])?.kind)
      .toBe("selftest");
    expect(runEndingFromAudit("FAILED", [ev("run.selftest", "failure")])?.kind).toBe("selftest");
  });

  // The server exempts an already-KILLED run from the terminal guard so a kill
  // whose teardown failed can be retried; the LAST attempt is the run's
  // containment truth, not the first.
  it("reads the LAST run.kill row — a successful retry is the current outcome", () => {
    const ending = runEndingFromAudit("KILLED", [
      ev("run.kill", "failure", { actor: "alice" }),
      ev("run.kill", "success", { actor: "bob" }),
    ]);
    expect(ending).toMatchObject({ kind: "killed", outcome: "success", actor: "bob" });
  });

  // The failing step's own words, the same error/reason/detail keys the CLI's
  // runFailureReason reads.
  it("carries the audit row's own reason as `detail`", () => {
    expect(
      runEndingFromAudit("FAILED", [ev("run.build", "failure", { data: { error: "pull denied" } })])?.detail,
    ).toBe("pull denied");
    expect(runEndingFromAudit("FAILED", [ev("run.build", "failure")])?.detail).toBeUndefined();
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

  // run.build is a BUILD failure (BYOI wrap / devcontainer / workspace image),
  // never a pull — and the builder's reason is already on the row, so the block
  // shows it rather than sending the operator to guess at a registry.
  it("image: says the image could not be built, and prints the builder's own error", () => {
    renderBlock("FAILED", [ev("run.build", "failure", { data: { error: "step 4/9: npm ci exited 1" } })]);
    expect(screen.getByText("What happened")).toBeInTheDocument();
    expect(screen.getByText(/The sandbox image could not be built/)).toBeInTheDocument();
    expect(screen.getByText("step 4/9: npm ci exited 1")).toBeInTheDocument();
    expect(screen.getByText("What to do")).toBeInTheDocument();
    expect(screen.getByText(/Fix the image or the devcontainer it builds from/)).toBeInTheDocument();
    expect(screen.getByTestId("run-failure-block")).toHaveAttribute("data-ending", "image");
  });

  it("selftest: says the run was refused before any task, and that there is nothing to clean up", () => {
    renderBlock("FAILED", [ev("run.selftest", "failure")]);
    expect(screen.getByText(/The image selftest failed/)).toBeInTheDocument();
    expect(screen.getByText(/there is nothing to clean up/)).toBeInTheDocument();
  });

  it("killed: names how far into the run it was killed, plus who and when on the audit row", () => {
    renderBlock("KILLED", [ev("run.kill", "success", { actor: "alice", actor_type: "human" })]);
    // CREATED -> run.kill EVENT_TIME, 26m apart.
    expect(screen.getByText(/An operator killed this run 26m 0s in\./)).toBeInTheDocument();
    expect(screen.getByText(/a killed run cannot resume/)).toBeInTheDocument();
    expect(screen.getByText(/run\.kill · success · alice/)).toBeInTheDocument();
  });

  // #1487: three outcomes, each saying only what Wardyn knows.
  it("killed, teardown confirmed: says what Wardyn did and claims nothing about upstream secrets", () => {
    renderBlock("KILLED", [ev("run.kill", "success", { actor: "alice", actor_type: "human" })]);
    expect(
      screen.getByText(
        "An operator killed this run 26m 0s in. Wardyn stopped and removed the sandbox and will issue it no more credentials.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByText("The audit row names who killed it and the reason they gave.")).toBeInTheDocument();
    expect(screen.getByText("Files written to a mounted workspace are still on the host; scratch is gone.")).toBeInTheDocument();
    expect(screen.getByText("Start a new run if the work still needs doing — a killed run cannot resume.")).toBeInTheDocument();
    expect(screen.queryByText(/torn down|stopped working at that moment|identity revoked/)).not.toBeInTheDocument();
    expect(screen.getByTestId("run-failure-block")).toHaveAttribute("data-evidence", "confirmed");
  });

  // A kill whose sandbox stop was queued for an offline runner revoked the credentials, but the sandbox
  // waits on the runner: neither "removed" nor a failed teardown.
  it("killed with the stop queued for an offline runner: says the sandbox stays until the runner reconnects", () => {
    const queued = ev("run.kill", "success", { data: { reason: "lifetime_cap", sandbox_stop: "queued" } });
    expect(runEndingFromAudit("KILLED", [queued])).toMatchObject({ evidence: "queued" });
    renderBlock("KILLED", [queued]);
    const name = runnerLabel(run("KILLED"));
    expect(screen.getByText(LOCAL_RUN.KILL_QUEUED("26m 0s", name))).toBeInTheDocument();
    expect(screen.getByText(LOCAL_RUN.KILL_QUEUED_FILES(name))).toBeInTheDocument();
    expect(screen.queryByText(/stopped and removed the sandbox|teardown step failed/)).not.toBeInTheDocument();
    expect(screen.getByTestId("run-failure-block")).toHaveAttribute("data-evidence", "queued");
  });

  // Ported from the review probe: a kill that failed partway printed "may
  // still be live" and "scratch is gone" together.
  it("killed with a runner failure: partial copy, teardown unconfirmed, and no scratch-is-gone claim", () => {
    renderBlock("KILLED", [
      ev("run.kill", "failure", { actor: "alice", actor_type: "human", data: { error: "runner_error: connection refused" } }),
    ]);
    expect(
      screen.getByText(
        "An operator killed this run 26m 0s in, and a teardown step failed. The state is KILLED, but teardown is not confirmed: the sandbox may still be live, and a credential it held may still work.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByText("runner_error: connection refused")).toHaveClass("font-mono");
    expect(screen.getByText("The audit row names the step that failed — read it before treating this run as contained.")).toBeInTheDocument();
    expect(screen.getByText("Killing it again re-runs the same teardown; the cascade is safe to repeat.")).toBeInTheDocument();
    expect(
      screen.getByText("Whether scratch files remain is not confirmed. Files written to a mounted workspace are still on the host."),
    ).toBeInTheDocument();
    expect(screen.queryByText(/scratch is gone/)).not.toBeInTheDocument();
    expect(screen.queryByText(/torn down/)).not.toBeInTheDocument();
    expect(screen.getByText(/run\.kill · failure · alice/)).toBeInTheDocument();
    expect(screen.getByTestId("run-failure-block")).toHaveAttribute("data-evidence", "partial");
  });

  // Ported from the review probe: a KILLED run whose trail has no run.kill row
  // invented the ending from the state alone and printed the clean claim.
  it("KILLED with no kill record: says it cannot confirm, with no actor, elapsed time or run.kill line", () => {
    renderBlock("KILLED", []);
    expect(
      screen.getByText(
        "This run is marked KILLED, but its audit trail has no kill record, so Wardyn can't confirm who killed it or whether teardown finished.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByText("Kill it again to run teardown once more; the cascade is safe to repeat.")).toBeInTheDocument();
    expect(screen.getByText("Start a new run if the work still needs doing — a killed run cannot resume.")).toBeInTheDocument();
    expect(screen.queryByText(/An operator killed this run/)).not.toBeInTheDocument();
    expect(screen.queryByText(/scratch is gone|torn down|stopped and removed/)).not.toBeInTheDocument();
    expect(screen.queryByText(/run\.kill/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Open audit trail/ })).toBeInTheDocument();
    expect(screen.getByTestId("run-failure-block")).toHaveAttribute("data-evidence", "unknown");
  });

  describe("per-kind credential lines", () => {
    const kill = (outcome: "success" | "failure") => [ev("run.kill", outcome, { actor: "alice", actor_type: "human" })];
    const held = (items: HeldCredential[]): HeldCredentials => ({ readable: true, items });
    const withHeld = (state: RunState, audit: AuditEvent[], h: HeldCredentials | undefined) =>
      render(<RunFailureBlock run={run(state)} audit={audit} held={h} onGoAudit={vi.fn()} />);

    it("a GitHub token it held: stays valid until it expires, on a confirmed kill", () => {
      withHeld("KILLED", kill("success"), held([{ kind: "github_token" }]));
      expect(screen.getByText("A GitHub token it already held stays valid until it expires, within an hour.")).toBeInTheDocument();
    });

    it("a git token and an SSH key name the kind and the host, and appear on a partial kill too", () => {
      withHeld(
        "KILLED",
        kill("failure"),
        held([
          { kind: "git_pat", host: "dev.azure.com" },
          { kind: "ssh_key", host: "github.com" },
        ]),
      );
      expect(
        screen.getByText("Wardyn can't revoke the git token this run used — it stays live until you rotate it on dev.azure.com."),
      ).toBeInTheDocument();
      expect(
        screen.getByText("Wardyn can't revoke the SSH key this run used — it stays live until you rotate it on github.com."),
      ).toBeInTheDocument();
    });

    it("an environment secret gets the rotation line", () => {
      withHeld("KILLED", kill("success"), held([{ kind: "env_secret" }]));
      expect(screen.getByText(ENV_SECRET_LINE)).toBeInTheDocument();
    });

    it("a file secret gets its own rotation line", () => {
      withHeld("KILLED", kill("success"), held([{ kind: "file_secret" }]));
      expect(screen.getByText("Wardyn can't revoke a secret this run received as a file — rotate it where it was issued.")).toBeInTheDocument();
    });

    it("no held credential of those kinds: no per-kind line at all", () => {
      withHeld("KILLED", kill("success"), held([]));
      expect(screen.queryByText(/stays valid until it expires|can't revoke|rotate/)).not.toBeInTheDocument();
      expect(screen.queryByText(UNREADABLE_LINE)).not.toBeInTheDocument();
    });

    it("facts that could not be read say so, on confirmed and partial", () => {
      withHeld("KILLED", kill("success"), { readable: false });
      expect(screen.getByText("Wardyn couldn't read which credentials this run held.")).toBeInTheDocument();
      cleanup();
      withHeld("KILLED", kill("failure"), { readable: false });
      expect(screen.getByText(UNREADABLE_LINE)).toBeInTheDocument();
    });

    it("facts still loading render no credential line", () => {
      withHeld("KILLED", kill("success"), undefined);
      expect(screen.queryByText(UNREADABLE_LINE)).not.toBeInTheDocument();
    });

    it("an unknown outcome has no per-kind lines: the trail it would read is the thing in doubt", () => {
      withHeld("KILLED", [], held([{ kind: "github_token" }]));
      expect(screen.queryByText(/stays valid until it expires/)).not.toBeInTheDocument();
    });
  });

  it("auto_stop: framed as the policy working, and names the field that sets it", () => {
    renderBlock("STOPPED", [ev("run.autostop", "success", { actor: "wardyn/lifecycle-reaper" })]);
    expect(screen.getByText(/This is the policy working, not a fault\./)).toBeInTheDocument();
    // The wire literal, in mono (§3) — must name the real field, not a
    // "never-reap lifecycle" control this console does not have.
    expect(screen.getByText("auto_stop_after_sec")).toHaveClass("font-mono");
    expect(screen.getByText("-1")).toHaveClass("font-mono");
  });

  // run.kill carries outcome "failure" when a teardown/revocation step failed
  // (runs_lifecycle.go): the run is KILLED but may not be contained, so the
  // clean-teardown copy — identity revoked, credential stopped working — would
  // be three false claims on the one surface that must not make them.
  it("killed with a failed teardown: says so instead of claiming containment", () => {
    renderBlock("KILLED", [ev("run.kill", "failure", { actor: "alice", actor_type: "human" })]);
    expect(screen.getByText(/a teardown step failed/)).toBeInTheDocument();
    expect(screen.getByText(/may still be live/)).toBeInTheDocument();
    expect(screen.queryByText(/identity revoked/)).not.toBeInTheDocument();
    expect(screen.getByText(/read it before treating this run as contained/)).toBeInTheDocument();
  });

  it("unknown: says there is no recorded cause, then the audit link — no invented cause, no generic advice", () => {
    renderBlock("FAILED", [ev("run.dispatch", "failure")]);
    expect(screen.getByTestId("run-failure-block")).toHaveAttribute("data-ending", "unknown");
    // #2919: the card is never a lone button; it states that nothing was recorded.
    expect(screen.getByText("What happened")).toBeInTheDocument();
    expect(screen.getByText(/no recorded cause for this failure/)).toBeInTheDocument();
    expect(screen.queryByText("What to do")).not.toBeInTheDocument();
    // Still actionable: the trail is one click away.
    expect(screen.getByRole("button", { name: /Open audit trail/ })).toBeInTheDocument();
  });

  // review R-01: run.failure_hint (D9's pre-agent-start class — never reaches
  // the audit trail at all, so `ending.kind` is "unknown" and ENDING_COPY has
  // no entry) is the prose for this ending, not silence — the block must
  // render it even though `copy` is undefined for an unknown ending.
  it("unknown WITH a run.failure_hint: the hint is the happened prose — no invented 'What to do'", () => {
    render(
      <RunFailureBlock
        run={{ ...run("FAILED"), failure_hint: "mount refused: workspace directory does not exist" }}
        audit={[ev("run.dispatch", "failure")]}
        onGoAudit={vi.fn()}
      />,
    );
    expect(screen.getByTestId("run-failure-block")).toHaveAttribute("data-ending", "unknown");
    expect(screen.getByText("What happened")).toBeInTheDocument();
    expect(
      screen.getByText("mount refused: workspace directory does not exist"),
    ).toBeInTheDocument();
    // Still no invented advice — the honesty rule holds for an unrecognised
    // cause even once it has a real reason attached.
    expect(screen.queryByText("What to do")).not.toBeInTheDocument();
  });

  // review R-12: a RECOGNISED ending already states this exact fact in its
  // own vocabulary (copy.happened + ending.detail) — printing run.failure_hint
  // too would be the same sentence three times. Gated on `!copy`, so a
  // failure_hint that happens to be set alongside a recognised cause is
  // silently absorbed by the copy that already covers it.
  it("image WITH a run.failure_hint: the hint paragraph does not also render — copy.happened already says it", () => {
    render(
      <RunFailureBlock
        run={{
          ...run("FAILED"),
          failure_hint: "the sandbox image could not be built",
        }}
        audit={[ev("run.build", "failure", { data: { error: "step 4/9: npm ci exited 1" } })]}
        onGoAudit={vi.fn()}
      />,
    );
    expect(screen.getByTestId("run-failure-block")).toHaveAttribute("data-ending", "image");
    expect(screen.getByText("What happened")).toBeInTheDocument();
    expect(screen.getByText(/The sandbox image could not be built/)).toBeInTheDocument();
    // The hint's own exact string does NOT appear as a second, separate
    // paragraph (copy.happened's sentence differs only by capitalization —
    // getByText would double-match on a case-insensitive lookup, so this
    // asserts the paragraph count directly instead).
    expect(
      screen.queryAllByText("the sandbox image could not be built"),
    ).toHaveLength(0);
  });

  it("Open audit trail hands off to the Audit tab", async () => {
    const onGoAudit = renderBlock("FAILED", [ev("run.build", "failure")]);
    await userEvent.click(screen.getByRole("button", { name: /Open audit trail/ }));
    expect(onGoAudit).toHaveBeenCalledTimes(1);
  });

  // 0.7.3 F7 moved "Start a run like this one" onto the run header (a strict
  // superset of the 3 endings this block explains) — the block itself carries
  // no clone door any more, on a killed run or otherwise.
  it("carries no second clone door — the run header is the one door", () => {
    renderBlock("KILLED", [ev("run.kill", "success", { actor: "alice", actor_type: "human" })]);
    expect(screen.queryByRole("button", { name: /Start a run like this one/i })).toBeNull();
  });
});

// f-f6 — sandbox-create and agent-start failures, graded by the stamped reason.
describe("dispatch failures in plain words (M2)", () => {
  const WRAPPED = "rpc error: code = Unknown desc = pull access denied for registry.example/team/agent:1.4";
  const STRIPPED = "pull access denied for registry.example/team/agent:1.4";

  it("grades each reason exactly; an unclassified run.create failure stays unknown", () => {
    expect(
      runEndingFromAudit("FAILED", [ev("run.create", "failure", { data: { error: "x", reason: "sandbox_create" } })])?.kind,
    ).toBe("sandbox_create");
    expect(
      runEndingFromAudit("FAILED", [ev("run.exec", "failure", { data: { error: "x", reason: "agent_start" } })])?.kind,
    ).toBe("agent_start");
    expect(runEndingFromAudit("FAILED", [ev("run.create", "failure", { data: { error: "sandbox_create refused" } })])?.kind).toBe(
      "unknown",
    );
  });

  it("relay_ip_fenced is its own ending, with its own copy", () => {
    const fenced = [ev("run.create", "failure", { data: { error: "k8s: proxy pod 10.244.1.7: relay_ip_fenced: x", reason: "relay_ip_fenced" } })];
    expect(runEndingFromAudit("FAILED", fenced)?.kind).toBe("relay_ip_fenced");
    renderBlock("FAILED", fenced);
    const block = screen.getByTestId("run-failure-block");
    expect(block.getAttribute("data-ending")).toBe("relay_ip_fenced");
    expect(block.textContent).toContain("its proxy was given a network address the runner still holds for an earlier run");
    expect(block.textContent).toContain("Start the run again. The policy and workspace are unchanged.");
  });

  it("sandbox_create renders its copy, the stripped words, and the unstripped text behind a closed disclosure", () => {
    renderBlock("FAILED", [ev("run.create", "failure", { data: { error: WRAPPED, reason: "sandbox_create" } })]);
    const block = screen.getByTestId("run-failure-block");
    expect(block.getAttribute("data-ending")).toBe("sandbox_create");
    expect(block.textContent).toContain(
      "The sandbox could not be created, so the run stopped before the agent started. There is no exit code because nothing ran.",
    );
    expect(block.textContent).toContain("Read the runner's message above — it names what it could not create or find.");
    expect(block.textContent).toContain("Fix that, then start a new run — the policy and workspace are unchanged.");
    const details = block.querySelector("details") as HTMLDetailsElement;
    expect(details.open).toBe(false);
    expect(details.textContent).toContain("Platform message");
    expect(details.textContent).toContain(WRAPPED);
    // The visible line outside the disclosure carries no wrapper.
    expect(block.textContent?.split(WRAPPED).length).toBe(2);
    expect(block.textContent).toContain(STRIPPED);
  });

  it("agent_start renders its copy and omits the disclosure when nothing was stripped", () => {
    renderBlock("FAILED", [
      ev("run.exec", "failure", { data: { error: "OCI runtime exec failed: unable to start", reason: "agent_start" } }),
    ]);
    const block = screen.getByTestId("run-failure-block");
    expect(block.getAttribute("data-ending")).toBe("agent_start");
    expect(block.textContent).toContain(
      "The sandbox was created, but the agent could not be started in it, so no task ran. There is no exit code because nothing ran.",
    );
    expect(block.textContent).toContain("Read the runner's message above — it says why the agent process would not start.");
    expect(block.querySelector("details")).toBeNull();
  });
});

// 0.9 (M-RN-3 RS5, RN-Q29): a run on a person's own computer. A runner's revocation ends its runs KILLED with a
// run.kill row naming the reason and whether the runner was connected; that is not an operator's kill, so it
// has no killer and its own arms. A clean kill of a local run says what the computer reported.
describe("RunFailureBlock: a run on a person's computer", () => {
  const localRun = (over: Partial<AgentRun> = {}): AgentRun => ({
    ...run("KILLED"),
    placement: "local",
    runner_id: "11111111-2222-3333-4444-555555555555",
    runner_name: "laptop-7",
    ...over,
  });
  const revoked = (online: boolean) =>
    ev("run.kill", "success", { actor: "wardynd", data: { reason: "runner_revoked", runner_online: online } });
  const renderLocal = (audit: AuditEvent[], over: Partial<AgentRun> = {}) =>
    render(<RunFailureBlock run={localRun(over)} audit={audit} onGoAudit={vi.fn()} />);

  it("runEndingFromAudit reads a revocation off the kill row, with whether the runner was online", () => {
    expect(runEndingFromAudit("KILLED", [revoked(true)])).toMatchObject({ kind: "killed", evidence: "confirmed", runnerRevoked: { online: true } });
    expect(runEndingFromAudit("KILLED", [revoked(false)])?.runnerRevoked).toEqual({ online: false });
    expect(runEndingFromAudit("KILLED", [ev("run.kill", "success", { data: { reason: "enough" } })])?.runnerRevoked).toBeUndefined();
    // Not the runner's reason as a "detail" under the heading: the arm says it in its own words.
    expect(runEndingFromAudit("KILLED", [revoked(true)])?.detail).toBeUndefined();
  });

  it("revoked while online: the runner was told to stop its network, the files stay there, and no operator is blamed", () => {
    renderLocal([revoked(true)]);
    expect(screen.getByText("laptop-7 was revoked")).toBeInTheDocument();
    expect(screen.getByText("This run's credentials were revoked, and laptop-7 was told to stop its network. Anything it wrote stays on laptop-7.")).toBeInTheDocument();
    expect(screen.getByText("The audit row for revoking laptop-7 names who did it.")).toBeInTheDocument();
    expect(screen.getByText("The sandbox and its files stay on laptop-7. Wardyn can't remove them; the computer's owner can.")).toBeInTheDocument();
    expect(screen.getByText("Start a new run if the work still needs doing — a killed run cannot resume.")).toBeInTheDocument();
    expect(screen.queryByText(/An operator killed this run/)).toBeNull();
    expect(screen.queryByText(/laptop-7 was revoked while offline/)).toBeNull();
  });

  // C4: the arm follows data.runner_online, and the two are alternatives, never stacked.
  it("revoked while offline: the replacement arm says it stops when it next connects", () => {
    renderLocal([revoked(false)]);
    expect(screen.getByText("laptop-7 was revoked while offline")).toBeInTheDocument();
    expect(screen.getByText("This run's credentials were revoked at once; laptop-7 stops its network when it next connects. Anything it wrote stays on laptop-7.")).toBeInTheDocument();
    expect(screen.queryByText("laptop-7 was revoked")).toBeNull();
    expect(screen.queryByText(/was told to stop its network/)).toBeNull();
  });

  it("offers no Revive, Extend or End run", () => {
    renderLocal([revoked(true)]);
    expect(screen.queryByRole("button", { name: /Revive|Extend|End run/ })).toBeNull();
    expect(screen.getByRole("button", { name: /Open audit trail/ })).toBeInTheDocument();
  });

  it("a revocation whose cascade did not finish is still a partial teardown first", () => {
    renderLocal([ev("run.kill", "failure", { data: { reason: "runner_revoked", runner_online: true, identity_error: "x" } })]);
    expect(screen.getByTestId("run-failure-block")).toHaveAttribute("data-evidence", "partial");
    expect(screen.queryByText("laptop-7 was revoked")).toBeNull();
  });

  // C5: the local complete arm, and the remote one unchanged.
  it("a clean kill of a local run uses what the computer reported and says the files are on it", () => {
    renderLocal([ev("run.kill", "success", { actor: "alice", actor_type: "human" })]);
    expect(screen.getByText("An operator killed this run 26m 0s in. laptop-7 reported it removed the sandbox; Wardyn issues it no more credentials.")).toBeInTheDocument();
    expect(screen.getByText("Files the run wrote to a folder on laptop-7 are still there; scratch is gone.")).toBeInTheDocument();
    expect(screen.queryByText(/Wardyn stopped and removed the sandbox/)).toBeNull();
    expect(screen.queryByText(/still on the host/)).toBeNull();
  });

  it("a remote kill reads exactly as before", () => {
    renderLocal([ev("run.kill", "success", { actor: "alice", actor_type: "human" })], { placement: "remote", runner_id: undefined, runner_name: undefined });
    expect(screen.getByText("An operator killed this run 26m 0s in. Wardyn stopped and removed the sandbox and will issue it no more credentials.")).toBeInTheDocument();
    expect(screen.getByText("Files written to a mounted workspace are still on the host; scratch is gone.")).toBeInTheDocument();
  });
});
