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
// Three cases are regression pins and say so: the sentence must never be
// chosen from the roster's DECLARED mechanism (F1), a run with no model
// credential gets no sentence at all (F4), and an unread /healthz makes no
// promise either way (F3).
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

// S1: the focus-return cases below mount the REAL door dialog, so its login
// pane is faked to one button — exactly as
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
import { RAIL, RAIL_CREDENTIAL, RAIL_RECORDING_ON, RECORDING_DISABLED_TITLE } from "../../wardyn/copy";
import { RAIL_MODEL_ACCESS } from "../../wardyn/model-access-copy";
import { ModelAccessBanner } from "../../wardyn/model-access-banner";
import { ModelAccessProvider, useModelAccessDoor } from "../../wardyn/model-access-context";
import { OperatorProvider } from "../../wardyn/operator-context";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { ADO } from "../../../lib/ado-entra-copy";
import { baseStatus } from "../../../lib/test-fixtures";
import { aheadByHours } from "../../../lib/test-clock";
import { AUTONOMY_RAIL, autonomyBoundSentence } from "../../../lib/governance-copy";
import { AUTONOMY_META } from "../../wardyn/autonomy-meta";
import type { AutonomyResolution } from "../../../lib/api/governance";
import type {
  ModelCredential,
  PreflightResult,
  SCMAccess,
  SetupHarnessTool,
  SetupModelAccess,
} from "../../../lib/types";

// U-15: through the constant, never a fourth typed copy of the sentence.
const RECORDING_ON = RAIL_RECORDING_ON;

// The server publishes credential_residency for ONE row shape, so these fixtures
// carry the declared mechanism the real wire carries — precisely so that a
// sentence wrongly picked off it (F1) would show up here.
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

function preflightWithAutonomy(autonomy?: AutonomyResolution): PreflightResult {
  return { setup_items: [], enforced_confinement_class: "CC1", autonomy };
}

type RailProps = Parameters<typeof railTree>[0];
function renderRail(props: RailProps) {
  const result = render(railTree(props));
  return { ...result, rerenderWith: (next: Partial<RailProps>) => result.rerender(railTree({ ...props, ...next })) };
}

function railTree(props: {
  agentRow?: SetupHarnessTool;
  preflightResult?: PreflightResult;
  showModelWarning?: boolean;
  governanceProfile?: string;
  showHoldNote?: boolean;
  /** Undefined (the default) mounts NO <ModelAccessProvider> at all — the
   *  fail-open contract every one of the ~15 pre-existing cases below relies
   *  on. Pass a value to grade a door for the rail's own tests. */
  modelAccess?: SetupModelAccess;
  operator?: boolean;
  onLaunch?: () => void;
  launchError?: string | null;
  /** #459: bumped on every failed launch — remounts the alert so a repeated,
   *  identical failure is re-announced. */
  launchErrorSeq?: number;
  preflightError?: string | null;
  preflightErrorSeq?: number;
  /** The server refused the launch for the caller's own model credential. */
  credentialRefused?: boolean;
  gitCredential?: SCMAccess;
  adoDialogOpen?: boolean;
  adoConnecting?: boolean;
  adoOrg?: string;
  adoBlockedUrl?: string | null;
  onAdoConfirm?: () => void;
  onAdoFallbackClick?: () => void;
  onAdoCancel?: () => void;
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
      governanceProfile={props.governanceProfile}
      showModelWarning={props.showModelWarning ?? false}
      startup="It starts."
      showHoldNote={props.showHoldNote ?? false}
      toolRules={null}
      launch={{
        onLaunch: props.onLaunch ?? (() => {}),
        disabled: false,
        spinning: false,
        inFlight: false,
        problem: null,
        error: props.launchError ?? null,
        errorSeq: props.launchErrorSeq ?? 0,
        credentialRefused: props.credentialRefused ?? false,
        warnings: [],
        onOpenRun: null,
      }}
      preflight={{
        error: props.preflightError ?? null,
        errorSeq: props.preflightErrorSeq ?? 0,
        // gitCredential rides the SAME preflight verdict as model_credential
        // does (RunRail derives both from preflight.result) — a synthetic
        // one when the test names only gitCredential, so the case reads as
        // "a preflight verdict carrying this fact" either way.
        result: props.gitCredential
          ? { setup_items: [], enforced_confinement_class: "CC1", ...props.preflightResult, git_credential: props.gitCredential }
          : (props.preflightResult ?? null),
      }}
      agentRow={props.agentRow}
      adoDialog={{
        open: props.adoDialogOpen ?? false,
        connecting: props.adoConnecting ?? false,
        org: props.adoOrg ?? "",
        blockedUrl: props.adoBlockedUrl ?? null,
        onConfirm: props.onAdoConfirm ?? (() => {}),
        onFallbackClick: props.onAdoFallbackClick ?? (() => {}),
        onCancel: props.onAdoCancel ?? (() => {}),
      }}
    />
  );
  if (props.modelAccess === undefined) {
    return <MemoryRouter>{rail}</MemoryRouter>;
  }
  return (
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
    </MemoryRouter>
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

// The six sentences, each keyed on what PREFLIGHT resolved. `mechanism` here is
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

  // F1 regression pin. The status row settles residency only for the per-user
  // Bedrock SSO shape; everywhere else it is absent while `mechanism` is still
  // populated. The rail must never pick its sentence off that DECLARED field —
  // doing so would read "AWS credentials sign inside the sandbox" over a
  // Claude sign-in for a compose deployment with a ~/.claude mount. A row that
  // settles nothing says nothing.
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

  // The negative control: nothing resolved — the state every user is in
  // before pressing anything — and the proxy sentence must never render.
  it("with neither source the proxy sentence never renders", async () => {
    renderRail({ agentRow: harnessRow() });
    expect(await screen.findByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.PROXY)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.PROXY_STAGED)).toBeNull();
  });

  // F4 regression pin. A shell command gets no model credential, so the screen
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

  // F3 regression pin. An unread /healthz is not evidence that recording is on,
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

// Finding 1: llmReady is a DEPLOYMENT fact, true the moment an admin saves a
// per_user roster row — the warning above must not stay silent for a member
// who has not signed in. The rail reads the shared model-access door for the
// claude-code row and states which per-person state the launcher is in, in
// the server's own words.
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
  // This pins that the action is NOT printed a second time — rename kept
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

  // setupBedrock grades llm_ready through the CALLER's own AWS scope, so a
  // never-signed-in per_user member reads SSOPresent=false -> llm_ready=false
  // -> showModelWarning=true on a deployment that unambiguously HAS a model
  // path — the admin's row exists. The two sentences must never render
  // stacked: the per-person line supersedes the deployment one whenever it
  // applies.
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

  // expired_signin's OTHER shape — a pin contradiction — carries a real
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
    const deadline = aheadByHours(3);
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

  // An older daemon sends `expiring` with no `deadline` field.
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

  // An OPERATOR under a dead SHARED row is the person who can
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

  // A codex-row negative control: model_access grades the claude-code row
  // alone (internal/api/modelaccess.go's modelAccessAgent) — a different
  // selected agent renders nothing whatever the door says.
  it("a codex row with an actionable claude-code model_access renders nothing", () => {
    renderRail({
      agentRow: { ...modelAccessRow(), id: "codex", display: "Codex" },
      modelAccess: { state: "not_configured", action: AGENTS.SIGN_IN_AWS },
    });
    expect(screen.queryByText(RAIL_MODEL_ACCESS.NOT_SIGNED_IN)).toBeNull();
    expect(screen.queryByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA })).toBeNull();
  });

  // A graded door that needs no attention must not change one byte of what
  // the rail renders.
  it("model_access live leaves today's rail byte-identical", () => {
    const withoutDoor = renderRail({ agentRow: harnessRow("sandbox") });
    const withLiveDoor = renderRail({
      agentRow: harnessRow("sandbox"),
      modelAccess: { state: "live" },
    });
    expect(withLiveDoor.container.innerHTML).toBe(withoutDoor.container.innerHTML);
  });

  // The fail-open contract: with no <ModelAccessProvider> above (every one of
  // the ~15 pre-existing cases in this file), the rail must render exactly
  // today's output — proven directly here rather than only by inference.
  it("with no ModelAccessProvider above, the rail offers no model-access line or control", () => {
    renderRail({ agentRow: harnessRow() });
    expect(screen.queryByText(RAIL_MODEL_ACCESS.NOT_SIGNED_IN)).toBeNull();
    expect(screen.queryByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA })).toBeNull();
  });

  // S1: focus after the door closes. Radix's FocusScope is still
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

    // A door someone ELSE opened (the rail never claimed it and
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

// Launch with a lapsed AWS SSO session. The server refuses
// the run (422, reason model_credential — before any run exists); the rail
// answers THAT refusal with the door and launches again when the sign-in
// completes. Launch is never pre-checked on the cached status: the server is
// the gate, and its answer is what opens the door.
describe("the launch door — the server's credential refusal opens the sign-in, and a completed sign-in launches again", () => {
  const afterFocusSettles = () => act(() => new Promise((r) => setTimeout(r, 0)));
  const pane = () => screen.queryByRole("button", { name: "fake pane" });
  // The NEGATIVES read the dialog itself: the pane is a lazy chunk, so "no
  // fake pane" is true for a tick whether or not the door opened.
  const dialog = () => screen.queryByRole("dialog");

  it("opens the door with no click, launches again exactly once on a completed sign-in, and returns focus to Launch", async () => {
    const onLaunch = vi.fn();
    // A STALE cache: the status still says live — the server's 422 is the fact.
    renderRail({
      agentRow: modelAccessRow(),
      modelAccess: { state: "live" },
      credentialRefused: true,
      banner: true,
      onLaunch,
    });
    expect(await screen.findByRole("button", { name: "fake pane" })).toBeInTheDocument();
    expect(onLaunch).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "fake pane" }));
    expect(onLaunch).toHaveBeenCalledTimes(1);
    await afterFocusSettles();
    expect(pane()).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Launch run" }));
  });

  it("Escape launches nothing; the sentence stays and the rail's own control appears once the status catches up", async () => {
    const onLaunch = vi.fn();
    renderRail({
      agentRow: modelAccessRow(),
      modelAccess: { state: "live" },
      // door.refresh() is called when the door opens, so the shell's next
      // answer is what the rail renders after the cancel.
      refreshTo: { state: "expired_signin", action: AGENTS.SIGN_IN_AWS },
      credentialRefused: true,
      launchError: "the server's sentence",
      banner: true,
      onLaunch,
    });
    await screen.findByRole("button", { name: "fake pane" });
    await userEvent.keyboard("{Escape}");
    await afterFocusSettles();
    expect(pane()).toBeNull();
    expect(onLaunch).not.toHaveBeenCalled();
    expect(screen.getByText("the server's sentence")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA })).toBeInTheDocument();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Launch run" }));
  });

  it("a member under a shared row gets the sentence and no door — the repair is the admin's", async () => {
    renderRail({
      agentRow: modelAccessRow({ credential_source: "shared" }),
      modelAccess: { state: "shared_expired", action: "ask your admin" },
      credentialRefused: true,
      launchError: "the server's sentence",
      banner: true,
    });
    await act(async () => {});
    expect(dialog()).toBeNull();
    expect(screen.getByText("the server's sentence")).toBeInTheDocument();
  });

  it("a refusal on a row that is not bedrock_sso opens nothing — an AWS sign-in repairs no api-key lane", async () => {
    renderRail({
      agentRow: modelAccessRow({ mechanism: "anthropic_api_key" as SetupHarnessTool["mechanism"], credential_source: "shared" }),
      modelAccess: { state: "live" },
      credentialRefused: true,
      banner: true,
      operator: true,
    });
    await act(async () => {});
    expect(dialog()).toBeNull();
  });

  it("once per click: a relaunch refused again does not reopen the door; a fresh Launch click re-arms it", async () => {
    const onLaunch = vi.fn();
    const r = renderRail({
      agentRow: modelAccessRow(),
      modelAccess: { state: "live" },
      credentialRefused: false,
      banner: true,
      onLaunch,
    });
    r.rerenderWith({ credentialRefused: true });
    await screen.findByRole("button", { name: "fake pane" });
    await userEvent.click(screen.getByRole("button", { name: "fake pane" }));
    expect(onLaunch).toHaveBeenCalledTimes(1);
    // The relaunch (through the door's callback, not the button) is refused
    // again: the screen clears the flag and sets it once more.
    r.rerenderWith({ credentialRefused: false });
    r.rerenderWith({ credentialRefused: true });
    await act(async () => {});
    expect(pane()).toBeNull();
    // The person clicks Launch themselves — that re-arms the door.
    await userEvent.click(screen.getByRole("button", { name: "Launch run" }));
    expect(onLaunch).toHaveBeenCalledTimes(2);
    r.rerenderWith({ credentialRefused: false });
    r.rerenderWith({ credentialRefused: true });
    expect(await screen.findByRole("button", { name: "fake pane" })).toBeInTheDocument();
  });

  it("a door the person opened themselves is left alone — completing it launches nothing, and it does not come back", async () => {
    const onLaunch = vi.fn();
    const r = renderRail({
      agentRow: modelAccessRow(),
      modelAccess: { state: "expired_signin", action: AGENTS.SIGN_IN_AWS },
      credentialRefused: false,
      banner: true,
      onLaunch,
    });
    await userEvent.click(screen.getByRole("button", { name: RAIL_MODEL_ACCESS.SIGN_IN_ARIA }));
    await screen.findByRole("button", { name: "fake pane" });
    r.rerenderWith({ credentialRefused: true });
    await userEvent.click(screen.getByRole("button", { name: "fake pane" }));
    expect(onLaunch).not.toHaveBeenCalled();
    // The click was consumed while the door was open: its closing must not
    // re-open it with a relaunch armed.
    await afterFocusSettles();
    expect(dialog()).toBeNull();
  });
});

// #93/#96 — the New Run rail's Autonomy section: what resolveRunAutonomy would
// cap this run at, once a preflight verdict is on screen.
describe("New run rail — the Autonomy section", () => {
  it("renders nothing before a preflight verdict is on screen", () => {
    renderRail({});
    expect(screen.queryByText(AUTONOMY_RAIL.HEADING)).toBeNull();
  });

  it("with no profile at all: the no-profile sentence and the no-limit chip", async () => {
    renderRail({ preflightResult: preflightWithAutonomy(undefined) });
    expect(await screen.findByText(AUTONOMY_RAIL.HEADING)).toBeInTheDocument();
    expect(screen.getByText(AUTONOMY_RAIL.NO_PROFILE)).toBeInTheDocument();
    expect(screen.queryByText(AUTONOMY_RAIL.NO_CAP)).toBeNull();
  });

  it("a profile with no rubric: the no-cap sentence, not the no-profile one", async () => {
    renderRail({ preflightResult: preflightWithAutonomy(undefined), governanceProfile: "Engineering" });
    expect(await screen.findByText(AUTONOMY_RAIL.HEADING)).toBeInTheDocument();
    expect(screen.getByText(AUTONOMY_RAIL.NO_CAP)).toBeInTheDocument();
    expect(screen.queryByText(AUTONOMY_RAIL.NO_PROFILE)).toBeNull();
  });

  it("a resolved level renders the level's friendly label and its one-cause sentence", async () => {
    renderRail({
      preflightResult: preflightWithAutonomy({
        level: "L1",
        posture: { egress: "sealed", secrets: "powerful", confinement: "CC1" },
        bound_by: ["secrets_powerful"],
      }),
    });
    expect(await screen.findByText(AUTONOMY_META.L1.label)).toBeInTheDocument();
    expect(screen.getByText(autonomyBoundSentence(["secrets_powerful"]))).toBeInTheDocument();
  });

  // Ruling 1 (#96 review): bound_by is a LIST, and a tie names EVERY cause —
  // the regression this pin exists to prevent is the rail reading bound_by[0]
  // alone and dropping the second (or third) tied row.
  it("a tie at the resolved level names EVERY bound_by cause, not just the first", async () => {
    const boundBy = ["secrets_powerful", "confinement_cc1"] as const;
    renderRail({
      preflightResult: preflightWithAutonomy({
        level: "L1",
        posture: { egress: "sealed", secrets: "powerful", confinement: "CC1" },
        bound_by: [...boundBy],
      }),
    });
    const sentence = autonomyBoundSentence([...boundBy]);
    expect(await screen.findByText(sentence)).toBeInTheDocument();
    expect(sentence).toContain("secrets");
    expect(sentence).toContain("barrier");
  });

  it("names the assigned governance profile beside a resolved level", async () => {
    renderRail({
      preflightResult: preflightWithAutonomy({
        level: "L2",
        posture: { egress: "open", secrets: "baseline", confinement: "CC1" },
        bound_by: ["egress_open"],
      }),
      governanceProfile: "Engineering",
    });
    expect(await screen.findByText(AUTONOMY_RAIL.PROFILE_LINE("Engineering"))).toBeInTheDocument();
  });

  it("a derived hold at L1 states the derived-hold note; a non-L1 level does not", async () => {
    const r = renderRail({
      preflightResult: preflightWithAutonomy({
        level: "L1",
        posture: { egress: "sealed", secrets: "powerful", confinement: "CC1" },
        bound_by: ["secrets_powerful"],
      }),
      showHoldNote: true,
    });
    expect(await screen.findByText(AUTONOMY_RAIL.DERIVED_HOLD_NOTE)).toBeInTheDocument();

    r.rerenderWith({
      preflightResult: preflightWithAutonomy({
        level: "L2",
        posture: { egress: "sealed", secrets: "powerful", confinement: "CC1" },
        bound_by: ["secrets_powerful"],
      }),
      showHoldNote: true,
    });
    expect(screen.queryByText(AUTONOMY_RAIL.DERIVED_HOLD_NOTE)).toBeNull();
  });
});

// #386's launch door — the Azure DevOps twin of the block above, but the
// rail here is presentational (adoDialog is screen-owned, see
// new-run-screen.test.tsx for the auto-open-on-422 + relaunch behaviour).
// These tests cover what the rail itself renders and wires.
describe("the Azure DevOps connect dialog and the git_credential preflight line", () => {
  it("renders nothing extra when there is no git_credential fact", () => {
    renderRail({});
    expect(screen.queryByText(ADO.PREFLIGHT_MISSING)).toBeNull();
    expect(screen.queryByRole("dialog", { name: ADO.LAUNCH_DIALOG_TITLE })).toBeNull();
  });

  // Review finding F4: on a deployment with no per-user Azure DevOps row (or
  // no Azure DevOps row at all), preflight never sends git_credential — a
  // shell run there must render no "Credentials" section, not an empty one.
  it("F4: a shell run with no git_credential fact renders no Credentials heading at all", () => {
    renderRail({});
    expect(screen.queryByText("Credentials")).toBeNull();
  });

  it("states PREFLIGHT_MISSING for a not_configured connection, before Launch is pressed", () => {
    renderRail({ gitCredential: { state: "not_configured" } });
    expect(screen.getByText(ADO.PREFLIGHT_MISSING)).toBeInTheDocument();
    expect(screen.getByText(ADO.PREFLIGHT_MISSING_SUB)).toBeInTheDocument();
  });

  it("says nothing for a live connection — no person name to compose PREFLIGHT_LIVE with", () => {
    renderRail({ gitCredential: { state: "live", source: "org" } });
    expect(screen.queryByText(ADO.PREFLIGHT_MISSING)).toBeNull();
  });

  // Review follow-up N5: a `live` gitCredential fact rendered nothing
  // (above), but showCredentials used to key on `!!gitCredential` — truthy
  // for `live` too — so a shell run with a live Azure DevOps connection and
  // no other credential to describe got an empty "Credentials" heading.
  it("N5: a live shell run with no other credential renders no empty Credentials heading", () => {
    renderRail({ gitCredential: { state: "live", source: "org" } });
    expect(screen.queryByText("Credentials")).toBeNull();
  });

  it("the dialog names the row's org (from the 422 body — F1) and offers Continue to Microsoft / Cancel", () => {
    // No preflight verdict at all — F1: the org comes from the 422 itself,
    // never from a git_credential fact that may not exist yet.
    renderRail({ adoDialogOpen: true, adoOrg: "https://dev.azure.com/contoso" });
    expect(screen.getByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE })).toBeInTheDocument();
    expect(screen.getByText(ADO.LAUNCH_DIALOG_BODY("https://dev.azure.com/contoso"))).toBeInTheDocument();
    expect(screen.getByRole("button", { name: ADO.CONNECT_CTA })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument();
  });

  it("a blocked popup shows the canon sentence and a plain fallback link to the sign-in URL (F9)", () => {
    renderRail({ adoDialogOpen: true, adoBlockedUrl: "/api/v1/scm/azure-devops/signin" });
    expect(screen.getByText(ADO.CONNECT_POPUP_BLOCKED)).toBeInTheDocument();
    const link = screen.getByRole("link", { name: ADO.CONNECT_POPUP_OPEN });
    expect(link).toHaveAttribute("href", "/api/v1/scm/azure-devops/signin");
    expect(link).toHaveAttribute("target", "_blank");
  });

  // Review follow-up N1: clicking the fallback link ALSO starts the poll
  // (alongside its own href navigation), so the dialog advances on return.
  it("N1: clicking the fallback link fires onFallbackClick", async () => {
    const onAdoFallbackClick = vi.fn();
    renderRail({
      adoDialogOpen: true,
      adoBlockedUrl: "/api/v1/scm/azure-devops/signin",
      onAdoFallbackClick,
    });
    await userEvent.click(screen.getByRole("link", { name: ADO.CONNECT_POPUP_OPEN }));
    expect(onAdoFallbackClick).toHaveBeenCalledTimes(1);
  });

  it("Continue to Microsoft calls onAdoConfirm; Cancel calls onAdoCancel", async () => {
    const onAdoConfirm = vi.fn();
    const onAdoCancel = vi.fn();
    renderRail({ adoDialogOpen: true, onAdoConfirm, onAdoCancel });
    await userEvent.click(screen.getByRole("button", { name: ADO.CONNECT_CTA }));
    expect(onAdoConfirm).toHaveBeenCalledTimes(1);
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onAdoCancel).toHaveBeenCalled();
  });

  it("closing the dialog (Escape) calls onAdoCancel too", async () => {
    const onAdoCancel = vi.fn();
    renderRail({ adoDialogOpen: true, onAdoCancel });
    await userEvent.keyboard("{Escape}");
    expect(onAdoCancel).toHaveBeenCalled();
  });

  it("the confirm button shows a spinner and disables while connecting", () => {
    renderRail({ adoDialogOpen: true, adoConnecting: true });
    expect(screen.getByRole("button", { name: ADO.CONNECT_CTA })).toBeDisabled();
  });
});

// #459 — the launch and preflight errors become role="alert" regions,
// announced on arrival, with an sr-only prefix spoken before the server's own
// (unchanged, still-visible) sentence.
describe("RunRail — failure lines are announced (#459)", () => {
  it("the launch error is an alert carrying the sr-only prefix and the server's sentence", () => {
    renderRail({ launchError: "the server's launch sentence" });
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent(RAIL.LAUNCH_ERROR_LABEL);
    expect(alert).toHaveTextContent("the server's launch sentence");
    // The sentence itself is unchanged and visible — only the prefix hides.
    expect(screen.getByText("the server's launch sentence")).toBeVisible();
  });

  it("the preflight error is an alert carrying the sr-only prefix and the server's sentence", () => {
    renderRail({ preflightError: "the server's preflight sentence" });
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent(RAIL.PREFLIGHT_ERROR_LABEL);
    expect(alert).toHaveTextContent("the server's preflight sentence");
  });

  it("a repeated, identical launch failure remounts the alert region (errorSeq keys it)", () => {
    const r = renderRail({ launchError: "same sentence", launchErrorSeq: 1 });
    const first = screen.getByRole("alert");
    r.rerenderWith({ launchError: "same sentence", launchErrorSeq: 2 });
    const second = screen.getByRole("alert");
    expect(second).not.toBe(first);
  });

  it("a repeated, identical preflight failure remounts the alert region (errorSeq keys it)", () => {
    const r = renderRail({ preflightError: "same sentence", preflightErrorSeq: 1 });
    const first = screen.getByRole("alert");
    r.rerenderWith({ preflightError: "same sentence", preflightErrorSeq: 2 });
    const second = screen.getByRole("alert");
    expect(second).not.toBe(first);
  });
});
