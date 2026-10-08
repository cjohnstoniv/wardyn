/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type {
  ConfinementClass,
  PreflightResult,
  PushRulesSpec,
  RunPolicySpec,
  SetupHarnessTool,
  SetupModelProvider,
  SetupProviderAccess,
} from "../../../lib/types";
import type * as React from "react";
import type { PolicyRef } from "../../../lib/api/health";
import type { ProviderGate } from "./model-provider-lane";
import type { LaunchIssue, NewRunPanelId } from "./new-run-launch-gates";

export interface RunRailProps {
  /** The panel on screen: an issue that panel prints beside its own control is
   *  not repeated above Launch. Absent, every issue is named here. */
  panel?: NewRunPanelId;
  /** Shows the panel that owns an issue and focuses its control. */
  onIssue?: (issue: LaunchIssue) => void;
  /** The click handler for a link that leaves the draft: it asks the unsaved
   *  guard first (use-unsaved-guard.tsx's useGuardedNavClick). */
  guardLink?: (to: string) => (e: React.MouseEvent) => void;
  /**
   * The governance profile bounding this caller, from GET /policies/default's
   * governance_profile_name. Undefined for a caller with no assignment — the
   * absent-row doctrine, and the section below simply does not render, so an
   * unassigned member's rail is byte-for-byte what it was.
   */
  governanceProfile?: string;
  /** GET /me's governance_contact: who owns the policy bounding this caller and
   *  how to ask for a change. Rendered beside the profile line; absent or null
   *  renders nothing. */
  governanceContact?: PolicyRef | null;
  /** The stored policy this run launches by reference, when there is one. */
  savedPolicy?: { name: string; spec: RunPolicySpec };
  /** The barrier the run requests (a separate wire field from the spec floor). */
  cc: ConfinementClass;
  /** An agent run with no model path launches, then fails its first model call. */
  showModelWarning: boolean;
  /** Launch is already blocked on the missing model: the block sentence speaks, so this
   *  rail adds neither the no-provider advice nor "Resolved at launch.". */
  modelBlocked?: boolean;
  /** What happens the moment this launches, in one sentence. */
  startup: string;
  /** Autonomous + held tool approvals: every call parks for a human. */
  showHoldNote: boolean;
  /** The run's tool_rules in one line, or null when it has none. */
  toolRules: string | null;
  /** The run's push_rules, from the SAME spec toolRules reads — the section
   *  renders only when pushRulesIsSet(pushRules) (#181). */
  pushRules?: PushRulesSpec;
  /** True for a batch/non-interactive run — nobody is here to answer a held
   *  push, so PUSH.RAIL_UNATTENDED joins the section when it renders at all. */
  unattended: boolean;
  launch: {
    /** Resolves to the server's refusal when the screen was gone before the
     *  answer came (use-launch.ts) — the strip shows it then (B9, #146). */
    onLaunch: () => void | Promise<string | void>;
    /** useDeferredBusy: disabled the instant it fires. */
    disabled: boolean;
    /** useDeferredBusy: the spinner arrives ~200ms later. */
    spinning: boolean;
    inFlight: boolean;
    /** Why Launch cannot be pressed — a disabled button that won't say is a dead end. */
    problem: string | null;
    /** `problem` as the issue it is: the panel and control that own it. */
    issue?: LaunchIssue | null;
    /** A fresh server refusal for THIS body holds Launch (use-launch's
     *  preflightBlock). No text of its own: the preflight alert below already
     *  shows the server's sentence, or the `problem` line the folded rows use. */
    preflightBlock?: boolean;
    /** An action that rides on the `problem` line (f-f5: "Connect →"). */
    problemLink?: { to: string; label: string };
    /** #922 review F5: an ADDITIONAL disable with no text of its own — the
     *  workspace picker's own advisory line (workspace-card.tsx) already
     *  names the reason, so Launch disables without the rail repeating the
     *  same sentence a second time. Optional so every other caller (this
     *  type's only other use is new-run-screen.tsx) is unaffected. */
    workspaceUnavailable?: boolean;
    /** #214: a settled probe reports this host can build no barrier at
     *  all — Launch disables for it (new-run-screen.tsx's own bit), and the
     *  rail states the reason here, beside Launch, with a route to the step
     *  that fixes it — never a tooltip on the disabled button. */
    noBarrier?: boolean;
    error: string | null;
    /** Bumped on every failed launch (see use-launch.ts) so a repeated,
     *  identical failure remounts the alert region and is re-announced (#459). */
    errorSeq: number;
    /** The launch refusal's `policy` (the error envelope), for the Request
     *  access remedy under the alert. */
    policy?: PolicyRef;
    /** The server refused this launch for the caller's own model credential (a
     *  422 carrying reason `model_credential`) — the one refusal a sign-in
     *  repairs, so the rail answers it with the door and launches again. */
    credentialRefused: boolean;
    /** The provider that refusal names (#532), "" when none: its door is the
     *  one that opens (#543). Optional so a caller with no provider block
     *  passes nothing. */
    refusedProvider?: string;
    /** The request Launch would send right now. A click-armed relaunch after
     *  sign-in only fires while this still equals the body the click was for. */
    body?: string | null;
    draftRevision?: number;
  };
  preflight: {
    /** Re-runs preflight on the current body — a preflight-origin sign-in's
     *  only action (it never launches). */
    onPreflight?: () => void | Promise<void>;
    /** Preflight's own model-credential refusal: the body it graded and the
     *  provider it names, "" when none. */
    refusal?: { body: string; provider: string } | null;
    /** A check is in flight (M1 S3). */
    checking?: boolean;
    /** The current body's last check was a 429 (M1 S3). */
    notChecked?: boolean;
    error: string | null;
    /** Same remount purpose as launch.errorSeq, for the preflight alert. */
    errorSeq: number;
    result: PreflightResult | null;
  };
  /**
   * The picked agent's /setup/status roster row — withheld by the screen for a
   * run that makes no model call (a shell command), so its absence is also how
   * this rail knows there is no model credential to describe.
   *
   * Its `credential_residency` is published for one row shape only (an enabled
   * per_user + bedrock_sso row) and is the only thing read off it here. The
   * row's `mechanism` is the declared lane and is never read: under a `shared`
   * row that lane is satisfied by a chain that fell through to a different,
   * resident one, and in legacy mode the field is empty — keying a sentence on it
   * rendered "AWS credentials sign inside the sandbox" over a Claude sign-in.
   */
  agentRow?: SetupHarnessTool;
  /**
   * #542 (design §5.6) — this run's model-provider picker, when a provider
   * block exists and at least one provider serves the picked agent (states
   * R1–R4, R6–R8; empty `candidates` or an absent prop both fall back to
   * today's CredentialFacts/showModelWarning shape, R9's existing path).
   * `candidates` and `access` are the screen's OWN /setup/status read —
   * mirrors agentRow's own withholding pattern — never the shell's, so a rail
   * mounted with no provider block above it renders exactly what it always
   * has. Selection is owned by the SCREEN (model-provider-lane.ts's
   * resolveProviderSelection): this rail renders and asks, it never picks.
   */
  modelProvider?: {
    candidates: SetupModelProvider[];
    access: SetupProviderAccess[] | undefined;
    selectedId: string | undefined;
    onChange: (id: string) => void;
    /** R7's info line, naming what the last agent switch changed; null every
     *  other state (R1/R2/R6/R8 stay silent — see resolveProviderSelection). */
    changeNote: string | null;
    /** R5b/R5c (#1052, #542 rail-gap packet) — model-provider-lane.ts's
     *  providerGate, undefined for the ordinary R1-R4/R6-R8 shapes and R9. */
    gate?: ProviderGate;
    /** The picked agent's human label (wizard-types.ts's agentLabel), for
     *  NOT_GRANTED/DEFAULT_OFF/DEFAULT_OFF_ONLY — the same label CHANGED
     *  already names in changeNote. */
    harnessLabel: string;
  };
  /** The Connect Azure DevOps launch-door dialog (§2.4, #386): owned by the
   *  screen (use-ado-launch-door.ts), rendered here. `org` comes from the
   *  422 body itself (review finding F1), never from a preflight fact — a
   *  422 can be the very first thing this caller hears about the row.
   *  `blockedUrl` is set when the browser refused the popup (review finding
   *  F9): a plain link to it renders instead. F8: confirming never
   *  relaunches — the person presses Launch themselves. */
  adoDialog: {
    open: boolean;
    connecting: boolean;
    org: string;
    blockedUrl: string | null;
    onConfirm: () => void;
    /** review follow-up N1: fires the same connect outcome as onConfirm, off
     *  the blocked-popup fallback link's own poll. */
    onFallbackClick: () => void;
    onCancel: () => void;
  };
}
