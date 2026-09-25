/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Split from new-run-rail.test.tsx (#195): that file was over the 800-line
// test gate. The credential/recording-fact describes (Appendix A finding 1,
// and Finding 1's per-person sign-in states) stay there; the launch door
// (server-refusal-opens-sign-in), the Autonomy section, and the Azure DevOps
// connect dialog describes — which don't touch those facts — live here.
import { describe, it, expect, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
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
import { RAIL_MODEL_ACCESS } from "../../wardyn/model-access-copy";
import { ModelAccessBanner } from "../../wardyn/model-access-banner";
import { ModelAccessProvider } from "../../wardyn/model-access-context";
import { OperatorProvider } from "../../wardyn/operator-context";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { ADO } from "../../../lib/ado-entra-copy";
import { baseStatus } from "../../../lib/test-fixtures";
import { AUTONOMY_RAIL, autonomyBoundSentence } from "../../../lib/governance-copy";
import { AUTONOMY_META } from "../../wardyn/autonomy-meta";
import type { AutonomyResolution } from "../../../lib/api/governance";
import type {
  PreflightResult,
  SCMAccess,
  SetupHarnessTool,
  SetupModelAccess,
} from "../../../lib/types";

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
  /** #725/T-65: the FULL roster StatusHost feeds `/setup/status` — defaults
   *  to `[agentRow]` (every pre-existing case's behaviour). A caller that
   *  needs a claude-code bedrock_sso row present WHILE `agentRow` names a
   *  different agent (a codex launch, say) passes both here — the one shape
   *  the default cannot produce. */
  harnesses?: SetupHarnessTool[];
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
      <StatusHost initial={props.modelAccess} refreshTo={props.refreshTo} agentRow={props.agentRow} harnesses={props.harnesses}>
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
  harnesses,
  children,
}: {
  initial: SetupModelAccess;
  refreshTo?: SetupModelAccess;
  agentRow?: SetupHarnessTool;
  harnesses?: SetupHarnessTool[];
  children: ReactNode;
}) {
  const [access, setAccess] = useState(initial);
  const status = baseStatus({ model_access: access, harnesses: harnesses ?? (agentRow ? [agentRow] : []) });
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

  // #725/T-65 — Codex launch refusal must not open "Sign in to AWS": the
  // door's own bedrockSSO/perUser facts grade the claude-code row ALONE
  // (modelAccessDoor mirrors modelAccessAgent server-side), regardless of
  // which agent THIS run picked. A deployment can carry a working
  // claude-code bedrock_sso per_user row at the same time a codex launch is
  // refused for its own, unrelated model_credential reason — an AWS
  // sign-in repairs neither.
  it("a codex launch's refusal opens no door, even with a claude-code bedrock_sso per_user row present", async () => {
    const onLaunch = vi.fn();
    renderRail({
      agentRow: { id: "codex", display: "Codex", has_gateway: true, has_login: false },
      harnesses: [modelAccessRow(), { id: "codex", display: "Codex", has_gateway: true, has_login: false }],
      modelAccess: { state: "live" },
      credentialRefused: true,
      banner: true,
      operator: true,
      onLaunch,
    });
    await act(async () => {});
    expect(dialog()).toBeNull();
    expect(onLaunch).not.toHaveBeenCalled();
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
  it("a shell run with no git_credential fact renders no Credentials heading at all", () => {
    // ticket: F4
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
  it("a live shell run with no other credential renders no empty Credentials heading", () => {
    // ticket: N5
    renderRail({ gitCredential: { state: "live", source: "org" } });
    expect(screen.queryByText("Credentials")).toBeNull();
  });

  it("the dialog names the row's org (from the 422 body) and offers Continue to Microsoft / Cancel", () => {
    // ticket: F1
    // No preflight verdict at all — F1: the org comes from the 422 itself,
    // never from a git_credential fact that may not exist yet.
    renderRail({ adoDialogOpen: true, adoOrg: "https://dev.azure.com/contoso" });
    expect(screen.getByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE })).toBeInTheDocument();
    expect(screen.getByText(ADO.LAUNCH_DIALOG_BODY("https://dev.azure.com/contoso"))).toBeInTheDocument();
    expect(screen.getByRole("button", { name: ADO.CONNECT_CTA })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument();
  });

  it("a blocked popup shows the canon sentence and a plain fallback link to the sign-in URL", () => {
    // ticket: F9
    renderRail({ adoDialogOpen: true, adoBlockedUrl: "/api/v1/scm/azure-devops/signin" });
    expect(screen.getByText(ADO.CONNECT_POPUP_BLOCKED)).toBeInTheDocument();
    const link = screen.getByRole("link", { name: ADO.CONNECT_POPUP_OPEN });
    expect(link).toHaveAttribute("href", "/api/v1/scm/azure-devops/signin");
    expect(link).toHaveAttribute("target", "_blank");
  });

  // Review follow-up N1: clicking the fallback link ALSO starts the poll
  // (alongside its own href navigation), so the dialog advances on return.
  it("clicking the fallback link fires onFallbackClick", async () => {
    // ticket: N1
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
