/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run screen's right-hand rail: what this run can actually do, and the
// one button that commits it.
//
// Split out of new-run-screen.tsx by the same seam new-run-primitives.tsx took
// (the file was past the 1000-line gate): this takes props and renders. It owns
// no screen state — every sentence it shows is derived UP in the screen, so the
// rail cannot describe one run while Launch sends another.
//
// It stays a FIXED 320px beside the form and wraps UNDER it below lg, sections
// side by side. Squeezing a 320px rail into a phone column is how the
// consequences of a choice end up unreadable exactly where they are hardest to
// scroll back to.
import * as React from "react";
import { Link } from "react-router-dom";
import { Loader2, TriangleAlert } from "lucide-react";
import type {
  ConfinementClass,
  ModelCredential,
  PreflightResult,
  RunPolicySpec,
  SCMAccess,
  SetupHarnessTool,
  SetupModelProvider,
  SetupProviderAccess,
} from "../../../lib/types";
import { Button, buttonVariants } from "../../ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { AutonomyChip, Chip, ConfinementChip, RiskBadge } from "../../wardyn/primitives";
import { CC_META } from "../../wardyn/cc-meta";
import { AUTONOMY_RAIL, autonomyBoundSentence, GOVERNANCE as GOV, MEMBER } from "../../../lib/governance-copy";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { ADO } from "../../../lib/ado-entra-copy";
import { PEOPLE } from "../../../lib/people-access-copy";
import { RAIL, RAIL_CREDENTIAL, RAIL_PROVIDER, RAIL_RECORDING_ON, RECORDING_DISABLED_TITLE, RUN } from "../../wardyn/copy";
import { CONNECTIONS } from "../../wardyn/copy/door";
import { useRecordingDisabled } from "../../../lib/hooks/use-recording-disabled";
import { RailSection } from "./new-run-primitives";
import { MODEL_ACCESS_AGENT } from "../../../lib/model-access";
import {
  accessStateFor,
  providerConnected,
  providerOptionLabel,
  providerResidency,
  type ProviderGate,
} from "./model-provider-lane";
import { absoluteTime, relativeTime } from "../../../lib/format";
import { RAIL_MODEL_ACCESS } from "../../wardyn/model-access-copy";
import {
  useClaimModelAccessDoor,
  useModelAccessDoor,
  useShellSetupStatus,
  type ModelAccessDoorHandle,
} from "../../wardyn/model-access-context";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";

interface RunRailProps {
  /**
   * The governance profile bounding this caller, from GET /policies/default's
   * governance_profile_name. Undefined for a caller with no assignment — the
   * absent-row doctrine, and the section below simply does not render, so an
   * unassigned member's rail is byte-for-byte what it was.
   */
  governanceProfile?: string;
  /** The stored policy this run launches by reference, when there is one. */
  savedPolicy?: { name: string; spec: RunPolicySpec };
  /** The barrier the run requests (a separate wire field from the spec floor). */
  cc: ConfinementClass;
  /** An agent run with no model path launches, then fails its first model call. */
  showModelWarning: boolean;
  /** What happens the moment this launches, in one sentence. */
  startup: string;
  /** Autonomous + held tool approvals: every call parks for a human. */
  showHoldNote: boolean;
  /** The run's tool_rules in one line, or null when it has none. */
  toolRules: string | null;
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
    error: string | null;
    /** Bumped on every failed launch (see use-launch.ts) so a repeated,
     *  identical failure remounts the alert region and is re-announced (#459). */
    errorSeq: number;
    /** The server refused this launch for the caller's own model credential (a
     *  422 carrying reason `model_credential`) — the one refusal a sign-in
     *  repairs, so the rail answers it with the door and launches again. */
    credentialRefused: boolean;
    /** The provider that refusal names (#532), "" when none: its door is the
     *  one that opens (#543). Optional so a caller with no provider block
     *  passes nothing. */
    refusedProvider?: string;
    /** The 201's advisory `warnings[]`, once Launch has actually fired
     *  (§5c.8) — rendered here, inline, instead of a toast. */
    warnings: string[];
    /** Set once a run launched with warnings: the screen stays put and this
     *  replaces Launch, so the member opens the run when they have read them.
     *  Null on every other state. A timed redirect races every other
     *  navigation off the screen, so this must replace Launch instead. */
    onOpenRun: (() => void) | null;
  };
  preflight: {
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
    /** R5c (#542 rail-gap packet) — model-provider-lane.ts's providerGate,
     *  undefined for the ordinary R1-R4/R6-R8 shapes, R9, AND R5b (not drawn
     *  — see providerGate's own doc comment). */
    gate?: ProviderGate;
    /** The picked agent's human label (wizard-types.ts's agentLabel), for
     *  DEFAULT_OFF/DEFAULT_OFF_ONLY — the same label CHANGED already names in
     *  changeNote. */
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

// CredentialFacts states where the model credential lands, and nothing wider —
// "Credentials" as a heading over "never written into the sandbox" was a
// universal claim only the model credential ever supported.
//
// The precedence, and why it is only two rungs. A current preflight verdict
// describes the exact body about to be launched, resolved lane and all, so it
// wins and everything below is read off it. Otherwise the only claim available
// is the one the roster row settles by itself — a per-user Bedrock SSO row,
// resident whatever the run carries — and that row is also the one case whose
// precise answer cannot be fetched, since Preflight 422s a member who has not
// signed in. Anything else is unresolved, and says so: there is no third rung
// that guesses.
function CredentialFacts({
  cred,
  agentRow,
  preflightRun,
}: {
  cred?: ModelCredential;
  agentRow?: SetupHarnessTool;
  /** Whether a current preflight verdict is on screen. */
  preflightRun: boolean;
}) {
  if (cred) {
    // Keyed on the resolved mechanism, never on the row's declared one.
    const bedrock = cred.residency === "sandbox" && cred.mechanism !== "anthropic_subscription";
    return (
      <>
        <CredentialLine>{credentialSentence(cred)}</CredentialLine>
        {bedrock && <AWSSignInChip perUser={cred.credential_source === "per_user"} />}
      </>
    );
  }
  if (agentRow?.credential_residency === "sandbox") {
    // The row-fixed case. per_user by construction — it is the only shape the
    // server publishes this field for.
    return (
      <>
        <CredentialLine>{RAIL_CREDENTIAL.SANDBOX_BEDROCK}</CredentialLine>
        <AWSSignInChip perUser />
      </>
    );
  }
  return (
    <>
      <CredentialLine>{RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH}</CredentialLine>
      {/* …and the way to find out, only while there is nothing to find it
          in. A current verdict that carries no `model_credential` — always so
          against a 0.7.4 daemon, and on 0.7.5 whenever the roster read failed or
          there is no store — puts this hint beside the result of pressing it: a
          promise that is false the moment it is followed. */}
      {!preflightRun && <CredentialLine>{RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT}</CredentialLine>}
    </>
  );
}

function CredentialLine({ children }: { children: React.ReactNode }) {
  return <p className="mt-1.5 text-xs leading-relaxed text-muted-foreground first:mt-0">{children}</p>;
}

// Whose AWS sign-in is resident — the difference between "my own session is in
// there" and "the admin's is", in the Barrier chip + tagline shape.
function AWSSignInChip({ perUser }: { perUser: boolean }) {
  return (
    <div className="mt-1.5">
      <Chip tone="neutral">
        {perUser
          ? RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER
          : RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_SHARED}
      </Chip>
    </div>
  );
}

// credentialSentence maps a resolved grade to the one sentence true of it.
function credentialSentence(cred: ModelCredential): string {
  switch (cred.residency) {
    case "proxy":
      return cred.staged_placeholder ? RAIL_CREDENTIAL.PROXY_STAGED : RAIL_CREDENTIAL.PROXY;
    case "sandbox":
      // The only two families that ever grade `sandbox`: every SigV4 Bedrock
      // lane, and the ~/.claude mount with proxy-side injection off.
      return cred.mechanism === "anthropic_subscription"
        ? RAIL_CREDENTIAL.SANDBOX_SUBSCRIPTION
        : RAIL_CREDENTIAL.SANDBOX_BEDROCK;
    case "image":
      return RAIL_CREDENTIAL.IMAGE;
    default:
      return RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH;
  }
}

// #542 — the residency line for a CHOSEN provider (R1, R4), read off its kind
// alone rather than a preflight verdict: with a provider block, every kind's
// residency is known before launch (model-provider-lane.ts's
// providerResidency; packet C's own R4 grouping). The chip stays because
// every provider credential in 0.8 is per person (D3 — no shared credential
// exists to contrast it with).
function ProviderResidencyLine({ provider }: { provider: SetupModelProvider }) {
  if (providerResidency(provider.kind) === "sandbox") {
    return (
      <>
        <CredentialLine>{RAIL_CREDENTIAL.SANDBOX_BEDROCK}</CredentialLine>
        <AWSSignInChip perUser />
      </>
    );
  }
  return <CredentialLine>{RAIL_CREDENTIAL.PROXY}</CredentialLine>;
}

// R3 — the selected candidate isn't connected yet: Launch stays enabled (a
// 422 opens this same door and relaunches, #543), but the person is told
// before pressing it rather than only after. Drawn for all five provider
// kinds providerWhatWord knows (bedrock_sso, custom_endpoint were PR #1036's;
// anthropic_api_key/openai_api_key/anthropic_subscription are the #542
// rail-gap packet's, canon.md's "R3 — selected, not connected"). `state`
// "not_applicable" (the shared admin token — provider_access.go's
// providerAccessMechanism) is not "connected" but is also not a person who
// could ever sign in or add a key, so it renders nothing rather than a door
// that can never work (#542 review finding F3).
function ProviderNotConnectedLine({
  provider,
  access,
  onSignIn,
}: {
  provider: SetupModelProvider;
  access: SetupProviderAccess[] | undefined;
  onSignIn: () => void;
}) {
  const state = accessStateFor(access, provider.id);
  if (providerConnected(state) || state === "not_applicable") return null;
  const name = provider.name ?? provider.id;
  if (provider.kind === "bedrock_sso") {
    return (
      <div className="mt-1.5">
        <p className="text-xs text-warning">{RAIL_PROVIDER.NOT_SIGNED_IN(name)}</p>
        <Button type="button" variant="outline" size="sm" className="mt-1.5" onClick={onSignIn}>
          {AGENTS.SIGN_IN_AWS}
        </Button>
      </div>
    );
  }
  if (provider.kind === "custom_endpoint") {
    return (
      <div className="mt-1.5">
        <p className="text-xs text-warning">{RAIL_PROVIDER.NO_TOKEN(name)}</p>
        <Button type="button" variant="outline" size="sm" className="mt-1.5" onClick={onSignIn}>
          {CONNECTIONS.ADD_TOKEN}
        </Button>
      </div>
    );
  }
  if (provider.kind === "anthropic_api_key" || provider.kind === "openai_api_key") {
    return (
      <div className="mt-1.5">
        <p className="text-xs text-warning">{RAIL_PROVIDER.NO_KEY(name)}</p>
        <Button type="button" variant="outline" size="sm" className="mt-1.5" onClick={onSignIn}>
          {CONNECTIONS.ADD_KEY}
        </Button>
      </div>
    );
  }
  if (provider.kind === "anthropic_subscription") {
    return (
      <div className="mt-1.5">
        <p className="text-xs text-warning">{RAIL_PROVIDER.NOT_SIGNED_IN_CLAUDE(name)}</p>
        <Button type="button" variant="outline" size="sm" className="mt-1.5" onClick={onSignIn}>
          {CONNECTIONS.SIGN_IN_CLAUDE}
        </Button>
      </div>
    );
  }
  return null;
}

// #542 — the Credentials section's provider half: R1 (one candidate, no
// picker — QC-1) through R3/R6/R7/R8 (a select, each option stating what the
// person provides and whether theirs is connected — QC-2), plus the rail-gap
// packet's R5c (`gate`, owner-approved 2026-09-25; R5b is not drawn — see
// providerGate's own doc comment). Rendered instead of CredentialFacts
// whenever the picked agent has at least one candidate OR a gate to name;
// RunRail falls back to CredentialFacts/showModelWarning for everything else
// (no provider block, or none serving this agent at all — R9).
/** The exact sentence R5c's gate names — DEFAULT_OFF_ONLY with no other
 *  candidate, DEFAULT_OFF otherwise — shared by ModelProviderSection's own
 *  inline line below and RunRail's launch-problem caption (F4, Opus review
 *  round 2): the caption is suppressed ONLY when launch.problem is exactly
 *  this string, never for some OTHER, higher-priority problem (an empty
 *  title, …) that happens to be showing while a gate is also active.
 *  undefined with no gate (R9's shape, or the ordinary R1-R4/R6-R8 ones). */
function gateSentence(modelProvider: RunRailProps["modelProvider"]): string | undefined {
  const gate = modelProvider?.gate;
  if (!modelProvider || gate?.kind !== "default_off") return undefined;
  const name = gate.provider.name ?? gate.provider.id;
  return modelProvider.candidates.length === 0
    ? RAIL_PROVIDER.DEFAULT_OFF_ONLY(name, modelProvider.harnessLabel)
    : RAIL_PROVIDER.DEFAULT_OFF(name, modelProvider.harnessLabel);
}

function ModelProviderSection({
  candidates,
  access,
  selectedId,
  onChange,
  changeNote,
  onSignIn,
  gate,
  harnessLabel,
}: {
  candidates: SetupModelProvider[];
  access: SetupProviderAccess[] | undefined;
  selectedId: string | undefined;
  onChange: (id: string) => void;
  changeNote: string | null;
  onSignIn: (provider: SetupModelProvider) => void;
  /** R5c — model-provider-lane.ts's providerGate. R5b is NOT drawn (Opus
   *  review round 2 — see providerGate's own doc comment): the console has no
   *  signal for "granted none" today, so `gate` is never "not_granted". */
  gate?: ProviderGate;
  harnessLabel: string;
}) {
  // R5c, no other candidate: no select renders at all, naming the disabled
  // default (Launch stays refused via RunRail's `problem`).
  if (gate?.kind === "default_off" && candidates.length === 0) {
    const name = gate.provider.name ?? gate.provider.id;
    return <p className="text-xs text-warning">{RAIL_PROVIDER.DEFAULT_OFF_ONLY(name, harnessLabel)}</p>;
  }
  // R1 — QC-1: one candidate needs no question with only one answer. Skipped
  // under R5c-with-others (gate set): the sole survivor is NOT the admin's
  // intended default, so it still gets an explicit ask, never a silent STATIC.
  if (!gate && candidates.length === 1) {
    const p = candidates[0];
    return (
      <>
        <p className="text-body font-medium text-foreground">{RAIL_PROVIDER.STATIC(p.name ?? p.id)}</p>
        <ProviderResidencyLine provider={p} />
      </>
    );
  }
  const selected = candidates.find((p) => p.id === selectedId);
  return (
    <>
      <label className="text-xs text-muted-foreground" htmlFor="nr-model-provider">
        {RAIL_PROVIDER.LABEL}
      </label>
      <Select value={selectedId ?? ""} onValueChange={onChange}>
        <SelectTrigger id="nr-model-provider" aria-label={RAIL_PROVIDER.LABEL} className="mt-1">
          <SelectValue placeholder={RAIL_PROVIDER.PLACEHOLDER} />
        </SelectTrigger>
        <SelectContent>
          {candidates.map((p) => (
            <SelectItem key={p.id} value={p.id}>
              {providerOptionLabel(p, access)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {/* R5c, other candidates remain: named until an explicit pick lands —
          selected clears it the same way it clears R7's changeNote below. */}
      {gate?.kind === "default_off" && !selected && (
        <p className="mt-1.5 text-xs text-warning">
          {RAIL_PROVIDER.DEFAULT_OFF(gate.provider.name ?? gate.provider.id, harnessLabel)}
        </p>
      )}
      {/* R7 — QC-3: a change the person didn't make is said once, and clears
          on the next one (the screen drops it as soon as the selection changes). */}
      {changeNote && (
        <p className="mt-1.5 rounded-md border border-info/25 bg-info-subtle px-2 py-1.5 text-xs text-info">
          {changeNote}
        </p>
      )}
      {selected && <ProviderNotConnectedLine provider={selected} access={access} onSignIn={() => onSignIn(selected)} />}
      {selected && <ProviderResidencyLine provider={selected} />}
    </>
  );
}

// ModelAccessLine — Finding 1: the rail states who (this launcher) needs to
// sign in, a second and independent fact from showModelWarning above (a
// deployment with no model path at all). RunRail withholds it entirely unless
// the selected agent is the one model_access grades and the door needs
// attention (showModelAccess) — a codex row or a live session renders nothing.
//
// `// ponytail:` this reads useModelAccessDoor() itself rather than taking the
// door as a prop threaded from new-run-screen.tsx: that screen is at its
// 1000-line file-size gate and gets a zero-line diff (the context exists
// exactly so a nested surface can reach the door with no prop-drilling).
function ModelAccessLine({ door, onSignIn }: { door: ModelAccessDoorHandle; onSignIn: () => void }) {
  const when = door.deadline ? relativeTime(door.deadline) : "";
  let sentence = "";
  let action = "";
  let title = "";
  // `expiring` is a state, not an alarm — muted
  // text, not the warning tint the other three states use.
  let warning = true;
  // The server's action only when it carries what the sentence and button
  // cannot — the pin-contradicted account/role pair — never the button's own
  // label repeated as prose.
  const serverAction = door.action && door.action !== AGENTS.SIGN_IN_AWS ? door.action : "";
  switch (door.state) {
    case "not_configured":
      sentence = RAIL_MODEL_ACCESS.NOT_SIGNED_IN;
      action = serverAction;
      break;
    case "expired_signin":
      sentence = RAIL_MODEL_ACCESS.EXPIRED;
      action = serverAction;
      break;
    case "expiring":
      warning = false;
      title = door.deadline ? absoluteTime(door.deadline) : "";
      // No separate action line — the deadline is in the sentence — except
      // against a daemon that sends no `deadline`: an older daemon's
      // `expiring` state would otherwise render nothing at all here while the
      // rail still claims the door — zero sign-in controls on /runs/new.
      // Mirrors the strip's own fallback.
      sentence = when ? RAIL_MODEL_ACCESS.EXPIRING(when) : "";
      action = when ? "" : door.action;
      break;
    case "shared_expired":
      // The one credential every run rides. Its admin reads their own repair
      // sentence, never the member's "ask them" line about themselves —
      // everybody else keeps the server's instruction.
      sentence = door.operator ? RAIL_MODEL_ACCESS.SHARED_ADMIN_EXPIRED : RAIL_MODEL_ACCESS.SHARED_EXPIRED;
      action = door.operator ? "" : door.action;
      break;
    default:
      // live, not_applicable, "" — RunRail's showModelAccess gate already
      // withholds this component for these, but a future daemon state this
      // console does not know says nothing rather than inventing a sentence.
      return null;
  }
  if (!sentence && !action) return null;
  return (
    <p
      className={
        "mb-1.5 rounded-md px-2 py-1.5 text-xs " +
        (warning ? "border border-warning/30 bg-warning-subtle text-foreground" : "text-muted-foreground")
      }
      title={title || undefined}
    >
      {/* Two separate text nodes (mirrors model-access-banner.tsx's
          modelAccessStripCopy rendering) — the server's action, when it
          renders, is a second fact beside ours, never appended into the same
          sentence. */}
      {sentence && <span>{sentence}</span>}
      {action && <span> {action}</span>}
      {/* The rail's own sign-in, under a distinct accessible name from the
          strip's/Getting Started's "Sign in to AWS" (U-13's actual rule is two
          controls with distinct names, not one hidden) — and hidden while the
          door dialog is open, so there is never a live control pointing at a
          dialog that is already on screen. Gated on door.actionable, not just
          needsAttention: a non-operator's shared_expired has nothing this
          viewer can repair. */}
      {door.actionable && !door.open && (
        <>
          {" "}
          <Button
            variant="link"
            size="sm"
            className="h-auto p-0 align-baseline text-xs"
            aria-label={RAIL_MODEL_ACCESS.SIGN_IN_ARIA}
            onClick={onSignIn}
          >
            {AGENTS.SIGN_IN_AWS}
          </Button>
        </>
      )}
    </p>
  );
}

// GitCredentialLine states what the rail knows about THIS caller's Azure
// DevOps connection before Launch is pressed (§2.4). Mirrors ModelAccessLine's
// "say nothing rather than invent" default: `live` needs no attention (and
// this rail has no person NAME to compose PREFLIGHT_LIVE with, so it does not
// try to), `shared_*`/`not_applicable` are nothing a launch-time line can fix,
// and `expiring` needs a deadline this deployment cannot compute yet
// (scmaccess.go's doc comment) — only `not_configured` renders.
function GitCredentialLine({ cred }: { cred?: SCMAccess }) {
  if (cred?.state !== "not_configured") return null;
  return (
    <p className="mb-1.5 rounded-md border border-warning/30 bg-warning-subtle px-2 py-1.5 text-xs text-foreground">
      <span>{ADO.PREFLIGHT_MISSING}</span> <span>{ADO.PREFLIGHT_MISSING_SUB}</span>
    </p>
  );
}

export function RunRail({
  governanceProfile,
  savedPolicy,
  cc,
  showModelWarning,
  startup,
  showHoldNote,
  toolRules,
  launch,
  preflight,
  agentRow,
  modelProvider,
  adoDialog,
}: RunRailProps) {
  // Both of finding 1's facts, read rather than asserted: where the model
  // credential lands, and whether this deployment records anything at all.
  // `recordingDisabled` is tri-state — undefined until /healthz answers.
  const cred = preflight.result?.model_credential;
  const gitCredential = preflight.result?.git_credential; // #386, informational — see GitCredentialLine
  const recordingDisabled = useRecordingDisabled();
  // Finding 1: model_access grades the claude-code row alone, so a shell
  // command or a different agent (codex) never reads this line whatever the
  // door says.
  const door = useModelAccessDoor();
  const showModelAccess = agentRow?.id === MODEL_ACCESS_AGENT && door.needsAttention;
  // Door ownership: the rail claims it for exactly as long
  // as it renders its own sign-in control, so the shell strip drops its
  // button here — no New Run exception — and keeps its sentence.
  useClaimModelAccessDoor(showModelAccess && door.actionable);

  // Focus returns to Launch, not to #main-content (which would drop the
  // member at the top of the form they were mid-way through), when this
  // rail's own control opened the door. The door owns the return target: the
  // rail's own sign-in control unmounts the moment the state it described
  // clears (a completed sign-in), so by the time the dialog's onCloseAutoFocus
  // runs, document.activeElement — what a bare openDoor() would have
  // captured — is a detached node and focusOpener() fails, falling through to
  // #main-content; a separate effect here racing Radix's own FocusScope exit
  // trap cannot reliably win either. Passing Launch explicitly as `returnTo`
  // makes it the captured opener directly.
  const launchRef = React.useRef<HTMLButtonElement>(null);

  // #542 — a provider block with at least one candidate for the picked agent,
  // OR a gate to name (R5c — the rail-gap packet), supersedes
  // CredentialFacts/showModelWarning entirely; with neither (no block, or none
  // serving this agent — R9) that legacy path is unchanged below.
  const hasProviderCandidates = !!modelProvider && (modelProvider.candidates.length > 0 || !!modelProvider.gate);
  const onProviderSignIn = (p: SetupModelProvider) => door.openDoor({ for: { provider: p.id }, returnTo: launchRef.current });

  // The server refused this click for the person's own model credential (422,
  // reason model_credential — the class failure-block.tsx grades a dead run by).
  // The door opens here, and the same launch fires again the moment the sign-in
  // lands, so a lapsed session costs one dialog rather than a trip to Getting
  // started. Launch stays the server's decision: nothing is pre-checked on the
  // cached status, which can be five minutes stale. Once per click: a relaunch
  // refused again (a pin contradiction the same identity cannot repair) leaves
  // the sentence and waits for the person. Never over a door someone else
  // opened: openDoor overwrites the opener, and the strip's focus contract
  // (model-access-banner.tsx) reads it on close — and a click is consumed on
  // its first evaluation, whatever the door's state then, so a door that
  // closes later (Escape, a sign-in started from the strip) never brings this
  // dialog back with a relaunch armed for a click the person has moved past.
  // A pending relaunch does survive leaving the page with the dialog open
  // (the dialog is the shell's): a sign-in completed then launches the run
  // that click asked for and lands on it.
  const onLaunchRef = React.useRef(launch.onLaunch);
  onLaunchRef.current = launch.onLaunch;
  const autoOpened = React.useRef(false);
  const { status: shellStatus } = useShellSetupStatus();
  const refusedProvider = launch.refusedProvider ?? "";
  const providerBlock = !!shellStatus?.model_providers;
  React.useEffect(() => {
    if (!launch.credentialRefused || autoOpened.current) return;
    autoOpened.current = true;
    if (door.open) return;
    if (refusedProvider) {
      // #543 (§5.8): the door of the provider the refusal names — never the
      // agent or provider selected on screen, which may have moved since the
      // click (#146's ruling). A provider this person has no door for opens
      // nothing (resolveDoor's null) and the sentence stands.
      door.openDoor({ for: { provider: refusedProvider }, returnTo: launchRef.current, onSignedIn: () => onLaunchRef.current() });
    } else {
      // A refusal naming no provider under a provider block (a sign-in renewal
      // that did not complete) has no door. #725/T-65: door.bedrockSSO grades
      // the claude-code row ALONE (modelAccessDoor mirrors
      // internal/api/modelaccess.go's modelAccessAgent) — it says nothing about
      // which agent THIS run picked, so a codex launch refused for its OWN
      // model_credential reason must not open "Sign in to AWS". Otherwise the
      // audience rule modelAccessDoor already states: a sign-in repairs a
      // bedrock_sso lane for its per_user owner, or for any operator (a shared
      // row); a member under a shared row keeps the server's sentence, no door.
      if (providerBlock || agentRow?.id !== MODEL_ACCESS_AGENT || !door.bedrockSSO || !(door.perUser || door.operator)) return;
      door.openDoor({ returnTo: launchRef.current, onSignedIn: () => onLaunchRef.current() });
    }
    // The strip and the line above catch up with what the server just said.
    void door.refresh();
  }, [launch.credentialRefused, refusedProvider, providerBlock, agentRow?.id, door]);

  // A run with no model credential to describe (a shell command — the screen
  // withholds agentRow for one), no model-access line and no warning to raise
  // has no Credentials section at all, rather than a heading over nothing.
  // review follow-up N5: gitCredential contributes only when GitCredentialLine
  // actually renders something for it (state "not_configured") — a `live`
  // gitCredential (nothing to say, see GitCredentialLine above) must not by
  // itself open an empty heading over a shell run with nothing else to show.
  const showCredentials =
    hasProviderCandidates || showModelWarning || !!cred || !!agentRow || showModelAccess || gitCredential?.state === "not_configured";
  // With no provider connected and nothing resolved, "Resolved at launch."
  // and the Preflight hint must not sit directly under "No model provider is
  // connected. This run launches; its first model call fails." Nothing
  // resolves at launch when there is nothing to resolve. A resolved credential
  // still states itself — that sentence is read off the verdict, not guessed.
  const showCredentialFacts = !!cred || (!!agentRow && !showModelWarning);
  return (
    // A sticky box is clamped by its containing block — with
    // ceiling + tool rules + 3 warnings (member/warnings path) the rail's
    // real content runs ~700-730px, below the fold at 1280x650 with no way
    // to reach Launch. Bounded to the viewport with its own scroll.
    //
    // 100vh - 5rem, not -3rem: the sticky container is app-shell.tsx's
    // <main> (its own overflow-y:auto scroller), which starts below the
    // h-14 (3.5rem/56px) header — sticky's `top-6` (1.5rem/24px) offset is
    // relative to that scroller, not the viewport, so the rail's stuck
    // position sits at 3.5rem+1.5rem = 5rem from the viewport top, not 1.5rem.
    <aside className="h-fit rounded-xl border border-border bg-card p-4 lg:sticky lg:top-6 lg:max-h-[calc(100vh-5rem)] lg:overflow-y-auto">
      <p className="mb-3 text-sm font-semibold text-foreground">What this run can do</p>

      {/* Below lg the rail sits under the form at full width, so its sections
          read across instead of stacking into a very tall column. */}
      <div className="grid gap-x-6 gap-y-3 sm:grid-cols-2 md:grid-cols-4 lg:grid-cols-1">
        {/* First, above Policy, because it bounds everything under it — a
            member's own spec and a saved policy they pick are both clamped to
            it. Frozen copy (§7.6), rendered only when a profile is actually
            assigned. */}
        {governanceProfile && (
          <RailSection title={GOV.CEILING_TITLE}>
            <p className="text-xs text-muted-foreground">{MEMBER.CEILING_PROFILE(governanceProfile)}</p>
          </RailSection>
        )}

        {savedPolicy && (
          <RailSection title="Policy">
            <p className="text-body font-medium text-foreground">{savedPolicy.name}</p>
            <p className="mt-0.5 text-xs text-muted-foreground">
              {RUN.SAVED_POLICY_GOVERNS(
                CC_META[savedPolicy.spec.min_confinement_class].label,
                savedPolicy.spec.allow_all_egress
                  ? "open egress"
                  : `${(savedPolicy.spec.allowed_domains ?? []).length} host${(savedPolicy.spec.allowed_domains ?? []).length === 1 ? "" : "s"} allowed`,
              )}
            </p>
          </RailSection>
        )}

        <RailSection title="Barrier">
          <div className="mb-1 flex items-center gap-2">
            <Chip tone="neutral">{CC_META[cc].label}</Chip>
            <span className="text-xs text-muted-foreground">· {CC_META[cc].tagline}</span>
          </div>
          <p className="text-xs text-muted-foreground">{CC_META[cc].doesntProtect}</p>
        </RailSection>

        {/* What resolveRunAutonomy (#97) would cap this run at — known only
            once a preflight verdict is on screen, exactly like the risk/
            confinement block at the bottom of this rail. `preflight.result.
            autonomy` is ABSENT (never a zero value) when nothing bound the
            run, which is indistinguishable from "no rubric on the assigned
            profile" and "no profile at all" — governanceProfile (already
            threaded above) is what tells those two apart. */}
        {preflight.result && (
          <RailSection title={AUTONOMY_RAIL.HEADING}>
            <div className="mb-1">
              <AutonomyChip level={preflight.result.autonomy?.level} />
            </div>
            {preflight.result.autonomy ? (
              <>
                {/* Ruling 1 (#96 review): bound_by is a LIST — a tie at the
                    resolved level names EVERY cause, not just the first. */}
                <p className="text-xs text-muted-foreground">
                  {autonomyBoundSentence(preflight.result.autonomy.bound_by ?? [])}
                </p>
                {showHoldNote && preflight.result.autonomy.level === "L1" && (
                  <p className="mt-1 text-xs font-medium text-foreground">{AUTONOMY_RAIL.DERIVED_HOLD_NOTE}</p>
                )}
                {governanceProfile && (
                  <p className="mt-1 text-xs text-muted-foreground">{AUTONOMY_RAIL.PROFILE_LINE(governanceProfile)}</p>
                )}
              </>
            ) : (
              <p className="text-xs text-muted-foreground">
                {governanceProfile ? AUTONOMY_RAIL.NO_CAP : AUTONOMY_RAIL.NO_PROFILE}
              </p>
            )}
          </RailSection>
        )}

        {showCredentials && (
        <RailSection title="Credentials">
          {/* Finding 1, above CredentialFacts: a per-person fact ("do I have a
              sign-in at all"), independent of showModelWarning below (a
              deployment fact — some model path exists at all). */}
          {showModelAccess && (
            <ModelAccessLine door={door} onSignIn={() => door.openDoor({ returnTo: launchRef.current })} />
          )}
          {/* The per-person line supersedes the deployment one when both would
              otherwise render: under a per_user row,
              setupBedrock grades llm_ready through the caller's own AWS
              scope, so a never-signed-in member reads SSOPresent=false ->
              Ready=false -> llm_ready=false -> showModelWarning=true on a
              deployment that unambiguously has a model path — the admin's
              row exists, this person just has not signed in yet. Stacking
              "No model provider is connected" under NOT_SIGNED_IN would be a
              false claim beside a true one. showModelAccess is the more
              specific fact whenever it applies; the deployment sentence
              still covers every other no-model-path shape (no per_user row
              at all, a shared credential nobody set up, a legacy daemon). */}
          {/* #542 — a provider block with a candidate for this agent (R1–R4,
              R6–R8) supersedes the legacy no-provider banner and
              CredentialFacts below entirely; R9 (no candidate at all) keeps
              exactly today's shape. */}
          {hasProviderCandidates && modelProvider && (
            <ModelProviderSection
              candidates={modelProvider.candidates}
              access={modelProvider.access}
              selectedId={modelProvider.selectedId}
              onChange={modelProvider.onChange}
              changeNote={modelProvider.changeNote}
              onSignIn={onProviderSignIn}
              gate={modelProvider.gate}
              harnessLabel={modelProvider.harnessLabel}
            />
          )}
          {!hasProviderCandidates && showModelWarning && !showModelAccess && (
            <p className="mb-1.5 rounded-md border border-warning/30 bg-warning-subtle px-2 py-1.5 text-xs text-foreground">
              {RAIL_MODEL_ACCESS.NO_PROVIDER}{" "}
              {/* The action that fills the gap rides next to the
                  need, not only in a footer. Links are --info, never teal. */}
              <Link to="/account" className="font-medium text-info hover:underline">
                {RAIL_MODEL_ACCESS.NO_PROVIDER_CTA}
              </Link>
            </p>
          )}
          {!hasProviderCandidates && showCredentialFacts && (
            <CredentialFacts cred={cred} agentRow={agentRow} preflightRun={!!preflight.result} />
          )}
          <GitCredentialLine cred={gitCredential} />
        </RailSection>
        )}

        {/* What actually happens when this launches. The startup choice is a
            real fork, and the rail is where this screen states consequences
            rather than leaving them to be discovered. */}
        <RailSection title="Startup">
          <p className="text-xs text-muted-foreground">{startup}</p>
          {showHoldNote && (
            <p className="mt-2 text-xs text-muted-foreground">
              Tool use parks as approvals — an operator decides each one.
            </p>
          )}
        </RailSection>

        {/* One line, and it names the tools: "3 rules" alone would say nothing
            about which calls still stop for a human. */}
        {toolRules && (
          <RailSection title="Tool rules">
            <p className="text-xs text-muted-foreground">{toolRules}</p>
          </RailSection>
        )}

        {/* A stock Helm install leaves persistence.enabled=false, so this
            promise was false out of the box — and wrong in both dangerous
            directions at once. The shared hook is the same /healthz read the
            Recordings library and the run cockpit make.

            Unknown renders nothing: a promise this specific may not be made
            from a /healthz read that has not landed, failed, or carried no
            recording component at all — and U-15: the whole section goes with
            it, as Credentials already does above. A bare "Recording" heading
            over nothing is a section that failed to load, and this rail is read
            as a checklist of what the run can do. */}
        {recordingDisabled !== undefined && (
          <RailSection title="Recording">
            <p className="text-xs text-muted-foreground">
              {recordingDisabled ? RECORDING_DISABLED_TITLE : RAIL_RECORDING_ON}
            </p>
          </RailSection>
        )}
      </div>

      {launch.error && (
        // key={launch.errorSeq}: a re-announce of the SAME sentence still
        // needs a fresh DOM node — an update in place is silent to a screen
        // reader on a live region (#459).
        <p
          key={launch.errorSeq}
          role="alert"
          className="mt-3 flex items-start gap-1.5 text-xs text-danger"
        >
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
          <span>
            <span className="sr-only">{RAIL.LAUNCH_ERROR_LABEL}</span> {launch.error}
          </span>
        </p>
      )}

      {/* §5c.8: the 201's advisory warnings, inline — the toast this replaced
          was gone the instant the run navigated away. */}
      {launch.warnings.length > 0 && (
        <div className="mt-3 rounded-md border border-warning/30 bg-warning-subtle px-2 py-1.5 text-xs text-warning">
          <p className="font-medium text-foreground">{AGENTS.LAUNCH_WARNING_TITLE}</p>
          <ul className="mt-1 list-disc space-y-0.5 pl-4">
            {launch.warnings.map((w, i) => (
              <li key={i}>{w}</li>
            ))}
          </ul>
        </div>
      )}

      {/* Preflight lives on the Policy panel, next to the document it checks —
          one button, not two competing ones. Its result stays here, beside
          Launch, because "what would be clamped" is the last thing read before
          committing. */}
      <div className="mt-4 flex gap-2">
        {launch.onOpenRun ? (
          // The run is launched — Launch has nothing left to do, and the one
          // teal here becomes the way on. Nothing navigates until it is clicked.
          <Button type="button" className="flex-1" onClick={launch.onOpenRun}>
            {AGENTS.OPEN_RUN_CTA}
          </Button>
        ) : (
          <Button
            ref={launchRef}
            type="button"
            className="flex-1"
            disabled={launch.disabled || !!launch.problem}
            onClick={() => {
              autoOpened.current = false;
              void launch.onLaunch();
            }}
          >
            {/* The icon slot always renders (never just on launching) so the
                has-[>svg] padding rule and the icon+gap width never change —
                toggling `invisible` cannot shift "Launch run" sideways the way
                mounting/unmounting the icon would. */}
            <Loader2 className={launch.spinning ? "size-4 animate-spin" : "size-4 animate-spin invisible"} />
            Launch run
          </Button>
        )}
      </div>
      {/* A disabled button that doesn't say why is a dead end: without
          client-side validation, an empty form would launch and the server's
          rejection would arrive after the fact. Suppressed ONLY when
          launch.problem IS the gate's (R5c's) own sentence — Opus review
          round 2, F4: ModelProviderSection above already names that exact
          fact inline, beside the select itself, so repeating it below would
          only echo it — but a DIFFERENT, higher-priority problem (an empty
          title, an unparseable policy, …) must still show here even while a
          gate is also active, since it's a separate reason nothing has
          launched yet. */}
      {launch.problem && !launch.inFlight && launch.problem !== gateSentence(modelProvider) && (
        <p className="mt-2 text-center text-xs text-muted-foreground">{launch.problem}</p>
      )}

      {preflight.error && (
        <p
          key={preflight.errorSeq}
          role="alert"
          className="mt-3 flex items-start gap-1.5 text-xs text-danger"
        >
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
          <span>
            <span className="sr-only">{RAIL.PREFLIGHT_ERROR_LABEL}</span> {preflight.error}
          </span>
        </p>
      )}
      {/* Unframed: a bordered box inside the rail card is a card in a card
          (CONSOLE-RULES §9). A divider is what separates a section from the
          section above it. */}
      {preflight.result && (
        <div className="mt-4 border-t border-border pt-3" data-testid="preflight-result">
          <div className="mb-1.5 flex flex-wrap items-center gap-2">
            {preflight.result.overall_risk && <RiskBadge level={preflight.result.overall_risk} />}
            <ConfinementChip value={preflight.result.enforced_confinement_class} />
          </div>
          {preflight.result.warnings && preflight.result.warnings.length > 0 ? (
            <ul className="list-disc space-y-0.5 pl-4 text-xs text-warning">
              {preflight.result.warnings.map((w, i) => (
                <li key={i}>{w}</li>
              ))}
            </ul>
          ) : (
            <p className="text-xs text-muted-foreground">{AGENTS.EFFECTIVE_NONE}</p>
          )}
        </div>
      )}

      {/* #386's launch door (§2.4): opened automatically on a git_credential
          422, and closable without launching — the screen owns the popup
          (use-ado-connect.ts), this dialog only asks. `org` is the 422
          body's own (review finding F1): this dialog can be the very first
          thing a caller sees about the row, before any preflight verdict. */}
      <Dialog open={adoDialog.open} onOpenChange={(open) => !open && adoDialog.onCancel()}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{ADO.LAUNCH_DIALOG_TITLE}</DialogTitle>
            <DialogDescription>{ADO.LAUNCH_DIALOG_BODY(adoDialog.org)}</DialogDescription>
          </DialogHeader>
          {/* review finding F9: the browser refused the popup outright — a
              plain link is the fallback, opened by the browser itself. N1:
              the same connect poll starts alongside that navigation, so the
              dialog still advances when the person comes back connected. */}
          {adoDialog.blockedUrl && (
            <p className="text-xs text-muted-foreground">
              {ADO.CONNECT_POPUP_BLOCKED}{" "}
              <a
                href={adoDialog.blockedUrl}
                target="_blank"
                rel="noopener noreferrer"
                className={buttonVariants({ variant: "outline", size: "sm" })}
                onClick={adoDialog.onFallbackClick}
              >
                {ADO.CONNECT_POPUP_OPEN}
              </a>
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={adoDialog.onCancel}>
              {PEOPLE.CANCEL}
            </Button>
            <Button type="button" onClick={adoDialog.onConfirm} disabled={adoDialog.connecting}>
              <Loader2 className={adoDialog.connecting ? "size-4 animate-spin" : "size-4 animate-spin invisible"} />
              {ADO.CONNECT_CTA}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </aside>
  );
}

// Re-exported so new-run-screen.tsx's existing "./new-run-rail" import line
// covers it too — that file sits at its own 1000-line gate.
export { useAdoLaunchDoor } from "./use-ado-launch-door";
