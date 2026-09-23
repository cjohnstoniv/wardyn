/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M7(b) — "What happened / What to do": a run that ended badly must state
// more than its state — the reason must not be left in the audit trail alone
// for an operator to find through the API.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import userEvent from "@testing-library/user-event";
import type { AgentRun, AuditEvent, RunState, SetupHarnessTool, SetupModelAccess } from "../../../lib/types";
import { runEndingFromAudit } from "../../../lib/api/audit";
import { RunFailureBlock } from "./failure-block";
import { ModelAccessBanner } from "../../wardyn/model-access-banner";
import { ModelAccessProvider } from "../../wardyn/model-access-context";
import { MODEL_ACCESS_RUN_DOOR } from "../../wardyn/model-access-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { OPEN_IN_USER_VIEW } from "../../wardyn/copy/console-view";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { baseStatus } from "../../../lib/test-fixtures";

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

  // An interactive BYOI run's selftest is warn-only (runs_dispatch.go's
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
    // created_at 10:00 -> run.kill 10:26.
    expect(screen.getByText(/An operator killed this run 26m 0s in\./)).toBeInTheDocument();
    expect(screen.getByText(/a killed run cannot resume/)).toBeInTheDocument();
    expect(screen.getByText(/run\.kill · success · alice/)).toBeInTheDocument();
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

  it("unknown: the audit link and nothing else — no invented cause, no generic advice", () => {
    renderBlock("FAILED", [ev("run.dispatch", "failure")]);
    expect(screen.getByTestId("run-failure-block")).toHaveAttribute("data-ending", "unknown");
    expect(screen.queryByText("What happened")).not.toBeInTheDocument();
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

// 0.7.6 Finding 3 — the failed run is the door
//
// A dispatch refusal names a destination ("sign in again under Settings →
// Model provider") to a person who was just interrupted, on a page that can
// mount the sign-in itself. The block shows the server's own sentence with
// the sign-in beside it — but only where a sign-in this viewer can complete
// would repair this run's lane.
const PER_USER_ROW: SetupHarnessTool = {
  id: "claude-code",
  display: "claude-code",
  has_gateway: false,
  has_login: true,
  enabled: true,
  mechanism: "bedrock_sso",
  credential_source: "per_user",
};

const REFUSAL =
  "this run's model access is configured as Amazon Bedrock (captured AWS SSO session), and that session can no longer be renewed — sign in to AWS from Getting started in the console, or from the sign-in banner the console shows on every page. Wardyn does not substitute a different model provider.";

/** The dispatch refusal's audit row: the sentence, its class, and the run's
 *  DECLARED lane. */
function credentialTrail(mechanism = "bedrock_sso"): AuditEvent[] {
  return [ev("run.create", "failure", { data: { error: REFUSAL, reason: "model_credential", mechanism } })];
}

function renderCredentialBlock({
  access = { state: "expired_signin", action: AGENTS.SIGN_IN_AWS } as SetupModelAccess,
  row = PER_USER_ROW,
  trail = credentialTrail(),
  principal = "alice",
  operator = false,
  onRefresh = vi.fn(),
  withStrip = false,
  adminView = false,
}: {
  access?: SetupModelAccess;
  row?: SetupHarnessTool;
  trail?: AuditEvent[];
  principal?: string;
  operator?: boolean;
  onRefresh?: () => void;
  withStrip?: boolean;
  adminView?: boolean;
} = {}) {
  render(
    <MemoryRouter initialEntries={["/runs/3b7f10c4"]}>
      <ModelAccessProvider
        status={baseStatus({ model_access: access, harnesses: [row] })}
        onRefresh={onRefresh}
      >
        <OperatorProvider operator={operator} securityOperator={operator} principal={principal}>
          {withStrip && (
            <div role="status">
              <ModelAccessBanner />
            </div>
          )}
          <RunFailureBlock
            run={{ ...run("FAILED"), failure_hint: REFUSAL }}
            audit={trail}
            onGoAudit={vi.fn()}
            adminView={adminView}
          />
        </OperatorProvider>
      </ModelAccessProvider>
    </MemoryRouter>,
  );
  return onRefresh;
}

const doorButton = () => screen.queryByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA });
const switchLink = () => screen.queryByRole("button", { name: OPEN_IN_USER_VIEW });

describe("the credential ending's door — only where a sign-in repairs THIS run", () => {
  it("renders the SERVER's sentence and the sign-in, and the sentence exactly once", () => {
    renderCredentialBlock();
    expect(screen.getByTestId("run-failure-block")).toHaveAttribute("data-ending", "credential");
    expect(screen.getAllByText(REFUSAL)).toHaveLength(1);
    expect(doorButton()).toBeInTheDocument();
    expect(screen.getByText(MODEL_ACCESS_RUN_DOOR.NOTE)).toBeInTheDocument();
    // No invented "What to do" — the server's sentence IS the reason.
    expect(screen.queryByText("What to do")).not.toBeInTheDocument();
  });

  // M-7 (admin-member-modes-design.md §4.6, §6) — the admin monitor carries
  // no credential door, even on the admin's own failed run (principal ===
  // created_by === "alice" here, same as the default sign-in case above).
  it("the admin view gets no button, even on the admin's own run — the switch link back to it instead", () => {
    renderCredentialBlock({ adminView: true });
    expect(doorButton()).not.toBeInTheDocument();
    expect(screen.queryByText(MODEL_ACCESS_RUN_DOOR.NOTE)).not.toBeInTheDocument();
    expect(switchLink()).toBeInTheDocument();
  });

  it("the admin view gives no switch link on a run that is not the admin's own, nor on the shared lane", () => {
    renderCredentialBlock({ adminView: true, principal: "bob", operator: true });
    expect(switchLink()).toBeNull();
    cleanup();
    renderCredentialBlock({ adminView: true, row: { ...PER_USER_ROW, credential_source: "shared" } });
    expect(switchLink()).toBeNull();
  });

  it("the user view never shows the switch link", () => {
    renderCredentialBlock();
    expect(switchLink()).toBeNull();
  });

  it("refreshes the door once on mount — the context can be five minutes stale", () => {
    const onRefresh = renderCredentialBlock();
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });

  it("a member under a SHARED credential gets the sentence and no button", () => {
    renderCredentialBlock({
      access: { state: "shared_expired", action: "Your admin's model credential expired — ask them to reconnect it" },
      row: { ...PER_USER_ROW, credential_source: "shared" },
    });
    expect(screen.getAllByText(REFUSAL)).toHaveLength(1);
    expect(doorButton()).toBeNull();
  });

  it("a refusal whose renewal merely did not complete gets no button (model access is live)", () => {
    renderCredentialBlock({ access: { state: "live" } });
    expect(screen.getAllByText(REFUSAL)).toHaveLength(1);
    expect(doorButton()).toBeNull();
  });

  // Codex #14: the reason covers EVERY declared lane; the viewer's model_access
  // grades Claude Code alone. Without the run's own lane on the row, a failed
  // Codex run whose owner also lacks an AWS sign-in would be offered one.
  it("another provider's refusal gets no AWS door, however actionable the Claude row is", () => {
    renderCredentialBlock({ trail: credentialTrail("openai_api_key") });
    expect(doorButton()).toBeNull();
  });

  // The roster moved since: signing in to AWS repairs nothing for an agent that
  // no longer reaches its model that way.
  it("a historical bedrock_sso refusal gets no door once the row's mechanism changed", () => {
    renderCredentialBlock({
      row: { ...PER_USER_ROW, mechanism: "anthropic_api_key", credential_source: "shared" },
    });
    expect(doorButton()).toBeNull();
  });

  // The door is the VIEWER's credential (round-2 general S5): an admin reading a
  // member's failed run must not be offered a sign-in that repairs nothing for
  // that run.
  it("a viewer who does not own the run gets no button", () => {
    renderCredentialBlock({ principal: "bob", operator: true });
    expect(screen.getAllByText(REFUSAL)).toHaveLength(1);
    expect(doorButton()).toBeNull();
  });

  // One primary recovery action per state per screen: the page that owns the
  // door takes the button, the strip keeps its sentence.
  it("claims the door, so the shell strip drops its button while the block has one", () => {
    renderCredentialBlock({ withStrip: true });
    expect(doorButton()).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
  });

  it("does not claim the door for a NON-credential ending", () => {
    render(
      <MemoryRouter initialEntries={["/runs/3b7f10c4"]}>
        <ModelAccessProvider
          status={baseStatus({
            model_access: { state: "expired_signin", action: AGENTS.SIGN_IN_AWS },
            harnesses: [PER_USER_ROW],
          })}
          onRefresh={vi.fn()}
        >
          <OperatorProvider operator={false} securityOperator={false} principal="alice">
            <div role="status">
              <ModelAccessBanner />
            </div>
            <RunFailureBlock run={run("FAILED")} audit={[ev("run.build", "failure")]} onGoAudit={vi.fn()} />
          </OperatorProvider>
        </ModelAccessProvider>
      </MemoryRouter>,
    );
    expect(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeInTheDocument();
  });
});
