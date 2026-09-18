/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Appendix A finding 1: the rail's two unconditional security claims.
//
// The Credentials line did not look at which mechanism the run's agent uses and
// the Recording line did not look at whether recording is enabled. Both are
// pinned here against the two facts the SERVER resolves, through the constants
// rather than through literals (a canon swap must not silently rewrite what
// these pin).
//
// Three cases are regression pins for the fix pass's review findings and say so:
// the sentence must never be chosen from the roster's DECLARED mechanism (F1), a
// run with no model credential gets no sentence at all (F4), and an unread
// /healthz makes no promise either way (F3).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { useState, type ReactNode } from "react";

// undefined = /healthz has not answered (or carried no recording component).
const recordingSelected = vi.hoisted(() => ({ value: "fs" as string | undefined }));
vi.mock("../../../lib/api/health", () => ({
  health: {
    health: () =>
      Promise.resolve({ components: { recording: { selected: recordingSelected.value } } }),
  },
}));

// S1 (review-1): the focus-return cases below mount the REAL door dialog, so
// its login pane is faked to one button — exactly as
// model-access-banner.test.tsx fakes it, which owns the pane's own suite;
// this file only needs "the sign-in completed".
vi.mock("../settings/harness-login-pane", () => ({
  HarnessLoginPane: ({ onDone }: { onDone: () => void }) => (
    <button type="button" onClick={onDone}>
      fake pane
    </button>
  ),
}));

import { RunRail } from "./new-run-rail";
import { RAIL_CREDENTIAL, RAIL_RECORDING_ON, RECORDING_DISABLED_TITLE } from "../../wardyn/copy";
import { RAIL_MODEL_ACCESS } from "../../wardyn/model-access-copy";
import { ModelAccessBanner } from "../../wardyn/model-access-banner";
import { ModelAccessProvider, useModelAccessDoor } from "../../wardyn/model-access-context";
import { OperatorProvider } from "../../wardyn/operator-context";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { baseStatus } from "../../../lib/test-fixtures";
import type {
  ModelCredential,
  PreflightResult,
  SetupHarnessTool,
  SetupModelAccess,
} from "../../../lib/types";

// U-15: through the constant, never a fourth typed copy of the sentence.
const RECORDING_ON = RAIL_RECORDING_ON;

// The server publishes credential_residency for ONE row shape, so these fixtures
// carry the declared mechanism the real wire carries — precisely so that a
// sentence picked off it would show up here. That it did not is how F1 shipped.
function harnessRow(residency?: "sandbox"): SetupHarnessTool {
  return {
    id: "claude-code",
    display: "Claude Code",
    has_gateway: true,
    has_login: true,
    mechanism: "bedrock_sso",
    credential_source: "per_user",
    credential_residency: residency,
  };
}

function preflightWith(cred: ModelCredential): PreflightResult {
  return { setup_items: [], enforced_confinement_class: "CC1", model_credential: cred };
}

function renderRail(props: {
  agentRow?: SetupHarnessTool;
  preflightResult?: PreflightResult;
  showModelWarning?: boolean;
  /** Undefined (the default) mounts NO <ModelAccessProvider> at all — the
   *  fail-open contract every one of the ~15 pre-existing cases below relies
   *  on. Pass a value to grade a door for the rail's own tests. */
  modelAccess?: SetupModelAccess;
  operator?: boolean;
  onLaunch?: () => void;
  launchError?: string | null;
  /** Mounted as a sibling INSIDE the same ModelAccessProvider — a test-only
   *  stand-in for a surface elsewhere in the shell that can close the shared
   *  door. */
  extra?: ReactNode;
  /** Mount the REAL <ModelAccessBanner/> (its dialog + Radix's actual
   *  onCloseAutoFocus/focusOpener contract) instead of nothing — S1's fix has
   *  to survive that real path, not a test-only door-closer. Also mounts a
   *  `#main-content` node, the banner's own fallback target. */
  banner?: boolean;
  /** What refresh() should answer with — simulates a completed sign-in
   *  actually clearing /setup/status (and unmounting the rail's own control),
   *  which is exactly the case S1's fix has to survive. */
  refreshTo?: SetupModelAccess;
}) {
  const rail = (
    <RunRail
      cc="CC1"
      showModelWarning={props.showModelWarning ?? false}
      startup="It starts."
      showHoldNote={false}
      toolRules={null}
      launch={{
        onLaunch: props.onLaunch ?? (() => {}),
        disabled: false,
        spinning: false,
        inFlight: false,
        problem: null,
        error: props.launchError ?? null,
        warnings: [],
        onOpenRun: null,
      }}
      preflight={{ error: null, result: props.preflightResult ?? null }}
      agentRow={props.agentRow}
    />
  );
  if (props.modelAccess === undefined) {
    return render(<MemoryRouter>{rail}</MemoryRouter>);
  }
  return render(
    <MemoryRouter>
      <StatusHost initial={props.modelAccess} refreshTo={props.refreshTo} agentRow={props.agentRow}>
        <OperatorProvider operator={!!props.operator} securityOperator={!!props.operator} principal="p@corp.example">
          {props.banner && <ModelAccessBanner />}
          {props.extra}
          {rail}
          {props.banner && (
            <main id="main-content" tabIndex={-1}>
              screen
            </main>
          )}
        </OperatorProvider>
      </StatusHost>
    </MemoryRouter>,
  );
}

/** The provider, with a status that can change on refresh() — every static
 *  (non-`banner`) case passes no `refreshTo` and behaves exactly as the
 *  fixed `baseStatus(...)` this replaces. */
function StatusHost({
  initial,
  refreshTo,
  agentRow,
  children,
}: {
  initial: SetupModelAccess;
  refreshTo?: SetupModelAccess;
  agentRow?: SetupHarnessTool;
  children: ReactNode;
}) {
  const [access, setAccess] = useState(initial);
  const status = baseStatus({ model_access: access, harnesses: agentRow ? [agentRow] : [] });
  return (
    <ModelAccessProvider status={status} onRefresh={() => refreshTo && setAccess(refreshTo)}>
      {children}
    </ModelAccessProvider>
  );
}

// The claude-code per_user row model_access grades — used by the Finding-1
// cases below, which read the DOOR rather than only the roster row.
function modelAccessRow(overrides: Partial<SetupHarnessTool> = {}): SetupHarnessTool {
  return { ...harnessRow(), ...overrides };
}

// THE SIX SENTENCES, each keyed on what PREFLIGHT resolved. `mechanism` here is
// the RESOLVED lane, which is the only mechanism the rail may ever read.
const credentialArms: { name: string; cred: ModelCredential; want: string }[] = [
  {
    name: "proxy",
    cred: { residency: "proxy", mechanism: "anthropic_api_key" },
    want: RAIL_CREDENTIAL.PROXY,
  },
  {
    name: "proxy with a staged placeholder",
    cred: { residency: "proxy", mechanism: "anthropic_subscription", staged_placeholder: true },
    want: RAIL_CREDENTIAL.PROXY_STAGED,
  },
  {
    name: "sandbox, Bedrock",
    cred: { residency: "sandbox", mechanism: "bedrock_sso", credential_source: "per_user" },
    want: RAIL_CREDENTIAL.SANDBOX_BEDROCK,
  },
  {
    name: "sandbox, subscription",
    cred: { residency: "sandbox", mechanism: "anthropic_subscription" },
    want: RAIL_CREDENTIAL.SANDBOX_SUBSCRIPTION,
  },
  { name: "image", cred: { residency: "image", mechanism: "none" }, want: RAIL_CREDENTIAL.IMAGE },
  { name: "unknown", cred: { residency: "unknown" }, want: RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH },
];

describe("New run rail — the two facts it used to assert (Appendix A finding 1)", () => {
  beforeEach(() => {
    recordingSelected.value = "fs";
  });

  for (const arm of credentialArms) {
    for (const recording of ["on", "disabled"] as const) {
      it(`${arm.name} credential, recording ${recording}`, async () => {
        recordingSelected.value = recording === "disabled" ? "none" : "fs";
        renderRail({ agentRow: harnessRow(), preflightResult: preflightWith(arm.cred) });

        expect(await screen.findByText(arm.want)).toBeInTheDocument();
        // Every OTHER sentence is absent: one residency, one claim.
        for (const other of credentialArms) {
          if (other.want !== arm.want) expect(screen.queryByText(other.want)).toBeNull();
        }
        await waitFor(() => {
          if (recording === "disabled") {
            expect(screen.getByText(RECORDING_DISABLED_TITLE)).toBeInTheDocument();
            expect(screen.queryByText(RECORDING_ON)).toBeNull();
          } else {
            expect(screen.getByText(RECORDING_ON)).toBeInTheDocument();
            expect(screen.queryByText(RECORDING_DISABLED_TITLE)).toBeNull();
          }
        });
      });
    }
  }

  // F1 REGRESSION PIN. The status row settles residency only for the per-user
  // Bedrock SSO shape; everywhere else it is absent while `mechanism` is still
  // populated. The rail used to pick its sentence off that DECLARED field, so a
  // compose deployment with a ~/.claude mount read "AWS credentials sign inside
  // the sandbox" over a Claude sign-in. A row that settles nothing says nothing.
  it("a row with no graded residency says only that it is not resolved yet", async () => {
    renderRail({ agentRow: harnessRow() });
    expect(await screen.findByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toBeInTheDocument();
    expect(screen.getByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_SHARED)).toBeNull();
  });

  // F1's other half: the subscription arm must win off the RESOLVED mechanism,
  // even though `sandbox` is overwhelmingly a Bedrock answer.
  it("a resolved subscription lane never renders the Bedrock sentence", async () => {
    renderRail({
      agentRow: harnessRow(),
      preflightResult: preflightWith({ residency: "sandbox", mechanism: "anthropic_subscription" }),
    });
    expect(await screen.findByText(RAIL_CREDENTIAL.SANDBOX_SUBSCRIPTION)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_SHARED)).toBeNull();
  });

  // The ROW-FIXED case — the only thing /setup/status publishes, and the field
  // report's own estate: a per-user AWS sign-in, stated with NO click.
  it("the row-fixed per_user Bedrock SSO row states residency with no Preflight", async () => {
    renderRail({ agentRow: harnessRow("sandbox") });
    expect(await screen.findByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeInTheDocument();
    expect(screen.getByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toBeNull();
  });

  // Preflight graded the body about to be launched; the row graded a shape. The
  // specific one wins, chip included.
  it("a current preflight verdict overrides the row-fixed row", async () => {
    renderRail({
      agentRow: harnessRow("sandbox"),
      preflightResult: preflightWith({ residency: "proxy", mechanism: "bedrock_bearer" }),
    });
    expect(await screen.findByText(RAIL_CREDENTIAL.PROXY)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER)).toBeNull();
  });

  // THE NEGATIVE CONTROL. Nothing resolved — the state every user is in before
  // pressing anything, and the state the old copy answered with "never written
  // into the sandbox".
  it("with neither source the proxy sentence never renders", async () => {
    renderRail({ agentRow: harnessRow() });
    expect(await screen.findByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.PROXY)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.PROXY_STAGED)).toBeNull();
  });

  // F4 REGRESSION PIN. A shell command gets no model credential, so the screen
  // withholds the agent row and there is nothing to say — not even "not resolved
  // yet", which would imply one is coming.
  it("a run with no model credential renders no Credentials section at all", async () => {
    renderRail({});
    await waitFor(() => expect(screen.getByText(RECORDING_ON)).toBeInTheDocument());
    for (const arm of credentialArms) expect(screen.queryByText(arm.want)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toBeNull();
    expect(screen.queryByText("Credentials")).toBeNull();
  });

  // U-4 (W6 blind lens). "Run Preflight to see where this run's model credential
  // will live." is a promise that is false the moment it is followed: a CURRENT
  // verdict that carries no `model_credential` (always so against a 0.7.4 daemon,
  // and on 0.7.5 whenever the roster read failed) leaves the rail telling the
  // reader to press the button they just pressed. The hint renders only while
  // there is no verdict at all.
  it("a current preflight verdict with no model_credential drops the Preflight hint", async () => {
    renderRail({
      agentRow: harnessRow(),
      preflightResult: { setup_items: [], enforced_confinement_class: "CC1" },
    });
    expect(await screen.findByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toBeNull();
  });

  // U-5 (W6 blind lens). A stock install before any key is added: llm_ready false
  // AND a roster row. One section said "No model provider is connected… its first
  // model call fails." AND "Resolved at launch." AND "Run Preflight…" — nothing
  // resolves at launch when nothing is connected.
  it("the no-provider warning replaces the credential facts rather than sitting beside them", async () => {
    renderRail({ agentRow: harnessRow(), showModelWarning: true });
    expect(await screen.findByText(/No model provider is connected/)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toBeNull();
  });

  // U-5's bound: a RESOLVED credential still states its residency beside the
  // warning — that sentence is read off the verdict, not guessed.
  it("…but a resolved credential is still stated beside the warning", async () => {
    renderRail({
      agentRow: harnessRow(),
      showModelWarning: true,
      preflightResult: preflightWith({ residency: "proxy", mechanism: "anthropic_api_key" }),
    });
    expect(await screen.findByText(RAIL_CREDENTIAL.PROXY)).toBeInTheDocument();
    expect(screen.getByText(/No model provider is connected/)).toBeInTheDocument();
  });

  // F3 REGRESSION PIN. An unread /healthz is not evidence that recording is on,
  // and this rail is where the promise about it gets made.
  it("recording says nothing until /healthz has actually answered", async () => {
    recordingSelected.value = undefined;
    renderRail({ agentRow: harnessRow("sandbox") });
    expect(await screen.findByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText(RECORDING_ON)).toBeNull());
    expect(screen.queryByText(RECORDING_DISABLED_TITLE)).toBeNull();
    // U-15: and the HEADING goes with it — a bare "Recording" over nothing reads
    // as a section that failed to load, on a rail read as a list of what the run
    // can do. Credentials already withholds its own heading the same way.
    expect(screen.queryByText("Recording")).toBeNull();
  });
});

// Finding 1 (Appendix A / plan lane ui-new-run-model-access): llmReady is a
// DEPLOYMENT fact, true the moment an admin saves a per_user roster row — so
// the warning above never reached the member who had not signed in. The rail
// now reads the shared model-access door for the claude-code row and states
// which per-person state the launcher is in, in the server's own words.
//
// A test-only sibling that closes the shared door — the real dialog lives in
// model-access-banner.tsx (the door lane's file, out of scope here).
function DoorCloser() {
  const door = useModelAccessDoor();
  return (
    <button type="button" onClick={door.closeDoor}>
      close door (test only)
    </button>
  );
}

describe("Finding 1 — the rail states WHO needs to sign in, not just whether a provider is connected", () => {
  // N3: this pins that the action is NOT printed a second time — rename kept
  // in step with what the assertion actually checks.
  it("not_configured states the person is not signed in — the server's action, byte-identical to the button label, is not printed a second time", async () => {
    renderRail({
      agentRow: modelAccessRow(),
      modelAccess: { state: "not_configured", action: AGENTS.SIGN_IN_AWS },
    });
    expect(await screen.findByText(RAIL_MODEL_ACCESS.NOT_SIGNED_IN)).toBeInTheDocument();
    // The server's action here is byte-identical to the button's own label, so
    // it must not be printed a second time as prose (S1) — exactly one control
    // carries that name.
    expect(screen.getAllByText(AGENTS.SIGN_IN_AWS)).toHaveLength(1);
  });

  // Live-walk finding (kind walk, case I): setupBedrock grades llm_ready
  // through the CALLER's own AWS scope, so a never-signed-in per_user member
  // reads SSOPresent=false -> llm_ready=false -> showModelWarning=true on a
  // deployment that unambiguously HAS a model path — the admin's row exists.
  // Before the fix, BOTH sentences rendered stacked: the true per-person line
  // and the false "No model provider is connected." The per-person line
  // supersedes the deployment one whenever it applies.
  it("a never-signed-in per_user member sees the per-person line and NOT the no-provider sentence", async () => {
    renderRail({
      agentRow: modelAccessRow(),
      modelAccess: { state: "not_configured", action: AGENTS.SIGN_IN_AWS },
      showModelWarning: true,
    });
    expect(await screen.findByText(RAIL_MODEL_ACCESS.NOT_SIGNED_IN)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_MODEL_ACCESS.NO_PROVIDER)).toBeNull();
    expect(screen.queryByText(RAIL_MODEL_ACCESS.NO_PROVIDER_CTA)).toBeNull();
  });

  // N2: expired_signin's OTHER shape — a pin contradiction — carries a real
  // account/role pair the sentence cannot say, so unlike not_configured this
  // one DOES render the server's action beside the sentence and the control.
  it("expired_signin with a pin-contradicted pair states EXPIRED, the server's pair, and offers the control", () => {
    const pin =
      "Your stored AWS session is for account 111111111111 / role Old; this row now allows 222222222222 / New — sign in again.";
    renderRail({
      agentRow: modelAccessRow(),
      modelAccess: { state: "expired_signin", action: pin },
    });
    expect(screen.getByText(RAIL_MODEL_ACCESS.EXPIRED)).toBeInTheDocument();
    expect(screen.getByText(pin)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA })).toBeInTheDocument();
  });

  it("renders the rail's OWN sign-in control under a DISTINCT accessible name, hidden while the door is open", async () => {
    renderRail({
      agentRow: modelAccessRow(),
      modelAccess: { state: "not_configured", action: AGENTS.SIGN_IN_AWS },
    });
    // The two names never collide: this getByRole must not match the rail's
    // own control.
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
    const signIn = screen.getByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA });
    await userEvent.click(signIn);
    // Opening the door hides the rail's own control — never a live control
    // pointing at a dialog that is already on screen.
    expect(screen.queryByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA })).toBeNull();
  });

  it("expiring renders the deadline line, and the run is never called refused", () => {
    const deadline = new Date(Date.now() + 3 * 60 * 60 * 1000).toISOString();
    renderRail({
      agentRow: modelAccessRow(),
      modelAccess: { state: "expiring", action: `Sign in again before ${deadline}`, deadline },
    });
    expect(screen.getByText(/^Your AWS sign-in lapses in /)).toBeInTheDocument();
    expect(screen.queryByText(/refused/i)).toBeNull();
    // No separate door.action line for `expiring` — the deadline is already IN
    // the sentence (S1 / W0-mock ruling 1; the lane's own state table said
    // "+ action", which the ruling names stale).
    expect(screen.queryByText(`Sign in again before ${deadline}`)).toBeNull();
  });

  // S3 (review-1): an older daemon sends `expiring` with no `deadline` field.
  // The sentence needs `{when}` and cannot form, but the state is still
  // needsAttention/actionable, so the rail still CLAIMS the door — without
  // the fallback below that leaves zero sign-in controls on /runs/new.
  it("expiring with no deadline on the wire falls back to the server's own action, not silence", () => {
    const action = "Sign in again before 2026-09-19T14:03:22Z";
    renderRail({
      agentRow: modelAccessRow(),
      modelAccess: { state: "expiring", action },
    });
    expect(screen.queryByText(/^Your AWS sign-in lapses/)).toBeNull();
    expect(screen.getByText(action)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA })).toBeInTheDocument();
  });

  it("a member's shared_expired states the member sentence and the admin's action, and offers no CTA", () => {
    const action = "Your admin's model credential expired — ask them to reconnect it";
    renderRail({
      agentRow: modelAccessRow({ credential_source: "shared" }),
      modelAccess: { state: "shared_expired", action },
    });
    expect(screen.getByText(RAIL_MODEL_ACCESS.SHARED_EXPIRED)).toBeInTheDocument();
    expect(screen.getByText(action)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA })).toBeNull();
  });

  // S2 (review-1): an OPERATOR under a dead SHARED row is the person who can
  // repair it — they read their OWN sentence, never the member's "ask them to
  // reconnect it" instruction about themselves, and no duplicate action line.
  it("an operator's shared_expired states the admin sentence alone, never the member's instruction", () => {
    const action = "Your admin's model credential expired — ask them to reconnect it";
    renderRail({
      agentRow: modelAccessRow({ credential_source: "shared" }),
      modelAccess: { state: "shared_expired", action },
      operator: true,
    });
    expect(screen.getByText(RAIL_MODEL_ACCESS.SHARED_ADMIN_EXPIRED)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_MODEL_ACCESS.SHARED_EXPIRED)).toBeNull();
    expect(screen.queryByText(action)).toBeNull();
    expect(screen.getByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA })).toBeInTheDocument();
  });

  // CODEX-ROW NEGATIVE. model_access grades the claude-code row alone
  // (internal/api/modelaccess.go's modelAccessAgent) — a different selected
  // agent renders nothing whatever the door says.
  it("a codex row with an actionable claude-code model_access renders nothing", () => {
    renderRail({
      agentRow: { ...modelAccessRow(), id: "codex", display: "Codex" },
      modelAccess: { state: "not_configured", action: AGENTS.SIGN_IN_AWS },
    });
    expect(screen.queryByText(RAIL_MODEL_ACCESS.NOT_SIGNED_IN)).toBeNull();
    expect(screen.queryByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA })).toBeNull();
  });

  // LIVE BYTE-IDENTICAL. A graded door that needs no attention must not change
  // one byte of what the rail renders today.
  it("model_access live leaves today's rail byte-identical", () => {
    const withoutDoor = renderRail({ agentRow: harnessRow("sandbox") });
    const withLiveDoor = renderRail({
      agentRow: harnessRow("sandbox"),
      modelAccess: { state: "live" },
    });
    expect(withLiveDoor.container.innerHTML).toBe(withoutDoor.container.innerHTML);
  });

  // THE FAIL-OPEN CONTRACT. With no <ModelAccessProvider> above (every one of
  // the ~15 pre-existing cases in this file), the rail must render exactly
  // today's output — proven directly here rather than only by inference.
  it("with no ModelAccessProvider above, the rail offers no model-access line or control", () => {
    renderRail({ agentRow: harnessRow() });
    expect(screen.queryByText(RAIL_MODEL_ACCESS.NOT_SIGNED_IN)).toBeNull();
    expect(screen.queryByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA })).toBeNull();
  });

  // S1 (review-1): focus after the door closes. Radix's FocusScope is still
  // mounted while onDone/onCancel run, so anything focused there is taken
  // back; onCloseAutoFocus is the callback that fires after the trap
  // releases, and every assertion below has to wait a macrotask for it —
  // same pattern as model-access-banner.test.tsx's afterFocusSettles.
  const afterFocusSettles = () => act(() => new Promise((r) => setTimeout(r, 0)));

  describe("where focus goes when the REAL door closes (S1 — review-1)", () => {
    // Escape leaves the state (and the rail's own control) unchanged — the
    // ordinary cancellation path — yet focus still lands on Launch, never on
    // the control that happened to be document.activeElement: the rail
    // always passes its own neighbour explicitly
    // (`door.openDoor(launchRef.current)`), one code path for every close
    // reason.
    it("Escape closes the real dialog and focus lands on Launch, not #main-content", async () => {
      renderRail({
        agentRow: modelAccessRow(),
        modelAccess: { state: "not_configured", action: AGENTS.SIGN_IN_AWS },
        banner: true,
      });
      await userEvent.click(screen.getByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA }));
      expect(screen.getByRole("button", { name: "fake pane" })).toBeInTheDocument();
      await userEvent.keyboard("{Escape}");
      await afterFocusSettles();
      expect(screen.queryByRole("button", { name: "fake pane" })).toBeNull();
      expect(document.activeElement).toBe(screen.getByRole("button", { name: "Launch run" }));
      expect(document.activeElement).not.toBe(document.getElementById("main-content"));
    });

    // The review's actual bug: a refresh that clears the state unmounts the
    // rail's OWN control — the element a bare openDoor() would have captured
    // as document.activeElement — before onCloseAutoFocus runs.
    // focusOpener() on that detached node fails and the banner falls through
    // to #main-content; passing Launch as `returnTo` makes it the captured
    // opener directly, so it wins regardless of what unmounted.
    it("a completed sign-in that clears the state still returns focus to Launch, not #main-content", async () => {
      renderRail({
        agentRow: modelAccessRow(),
        modelAccess: { state: "not_configured", action: AGENTS.SIGN_IN_AWS },
        refreshTo: { state: "live" },
        banner: true,
      });
      await userEvent.click(screen.getByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA }));
      await userEvent.click(screen.getByRole("button", { name: "fake pane" }));
      await afterFocusSettles();
      // The rail's own control is gone — the state it described cleared.
      expect(screen.queryByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA })).toBeNull();
      expect(document.activeElement).toBe(screen.getByRole("button", { name: "Launch run" }));
      expect(document.activeElement).not.toBe(document.getElementById("main-content"));
    });

    // NEGATIVE: a door someone ELSE opened (the rail never claimed it and
    // never rendered its own control here — showModelAccess false) must not
    // steal focus to Launch when it closes.
    it("does not move focus when the door was never opened from this rail", async () => {
      renderRail({
        agentRow: harnessRow(),
        modelAccess: { state: "live" },
        extra: <DoorCloser />,
      });
      const launch = screen.getByRole("button", { name: "Launch run" });
      launch.blur();
      await userEvent.click(screen.getByRole("button", { name: "close door (test only)" }));
      expect(launch).not.toHaveFocus();
    });
  });
});
