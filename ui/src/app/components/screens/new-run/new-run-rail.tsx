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
  PreflightResult,
  PushRulesSpec,
  RunPolicySpec,
  SCMAccess,
  SetupHarnessTool,
  SetupModelProvider,
  SetupProviderAccess,
} from "../../../lib/types";
import { PUSH } from "../../wardyn/copy/push";
import { Button, buttonVariants } from "../../ui/button";
import { AutonomyChip, Chip, ConfinementChip, RiskBadge } from "../../wardyn/primitives";
import { CC_META } from "../../wardyn/cc-meta";
import { AUTONOMY_RAIL, autonomyBoundSentence, GOVERNANCE as GOV, MEMBER } from "../../../lib/governance-copy";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { ADO } from "../../../lib/ado-entra-copy";
import type { SCMAccessPAT } from "../../../lib/types/ado-pat";
import { PEOPLE } from "../../../lib/people-access-copy";
import { NO_BARRIER, RAIL, RAIL_PROVIDER, RAIL_RECORDING_ON, RECORDING_DISABLED_TITLE, RUN } from "../../wardyn/copy";
import { useRecordingDisabled } from "../../../lib/hooks/use-recording-disabled";
import { useOperator } from "../../wardyn/operator-context";
import { useViewAccess } from "../../wardyn/console-view";
import { RailSection } from "./new-run-primitives";
import { CredentialFacts, ModelProviderSection } from "./new-run-rail-credentials";
import type { ProviderGate } from "./model-provider-lane";
import { RAIL_MODEL_ACCESS } from "../../wardyn/model-access-copy";
import { useModelAccessDoor } from "../../wardyn/model-access-context";
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
    /** The server refused this launch for the caller's own model credential (a
     *  422 carrying reason `model_credential`) — the one refusal a sign-in
     *  repairs, so the rail answers it with the door and launches again. */
    credentialRefused: boolean;
    /** The provider that refusal names (#532), "" when none: its door is the
     *  one that opens (#543). Optional so a caller with no provider block
     *  passes nothing. */
    refusedProvider?: string;
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

/** The exact sentence R5b/R5c's gate names — NOT_GRANTED for R5b;
 *  DEFAULT_OFF_ONLY with no other candidate, DEFAULT_OFF otherwise, for R5c —
 *  shared by ModelProviderSection's own inline line (new-run-rail-credentials.tsx)
 *  and RunRail's launch-problem caption (F4, Opus review round 2): the caption
 *  is suppressed ONLY when launch.problem is exactly this string, never for
 *  some OTHER, higher-priority problem (an empty title, …) that happens to be
 *  showing while a gate is also active. undefined with no gate (R9's shape, or
 *  the ordinary R1-R4/R6-R8 ones). */
function gateSentence(modelProvider: RunRailProps["modelProvider"]): string | undefined {
  const gate = modelProvider?.gate;
  if (!modelProvider || !gate) return undefined;
  if (gate.kind === "not_granted") return RAIL_PROVIDER.NOT_GRANTED(modelProvider.harnessLabel);
  const name = gate.provider.name ?? gate.provider.id;
  return modelProvider.candidates.length === 0
    ? RAIL_PROVIDER.DEFAULT_OFF_ONLY(name, modelProvider.harnessLabel)
    : RAIL_PROVIDER.DEFAULT_OFF(name, modelProvider.harnessLabel);
}

// GitCredentialLine states what the rail knows about THIS caller's Azure
// DevOps connection before Launch is pressed (§2.4). Its default is "say
// nothing rather than invent": `live` needs no attention (and
// this rail has no person NAME to compose PREFLIGHT_LIVE with, so it does not
// try to), `shared_*`/`not_applicable` are nothing a launch-time line can fix,
// and `expiring` needs a deadline this deployment cannot compute yet
// (scmaccess.go's doc comment) — only `not_configured` renders.
//
// A row that creates a token per run says its own piece on the Policy section
// (AdoLaunchNote): its launch is refused rather than prompted, so "you'll be
// asked to connect" would be wrong here.
const gitCredentialSpeaks = (cred?: SCMAccess) =>
  cred?.state === "not_configured" && (cred as SCMAccessPAT).token_mode !== "minted_pat";

function GitCredentialLine({ cred }: { cred?: SCMAccess }) {
  if (!gitCredentialSpeaks(cred)) return null;
  return (
    <p className="mb-1.5 rounded-md border border-warning/30 bg-warning-subtle px-2 py-1.5 text-xs text-foreground">
      <span>{ADO.PREFLIGHT_MISSING}</span> <span>{ADO.PREFLIGHT_MISSING_SUB}</span>
    </p>
  );
}

// pushRulesIsSet mirrors types.PushRulesSpec.IsSet() (internal/types/policy.go)
// EXACTLY: what "the policy has push rules" means everywhere it's asked,
// which is NOT a bare truthiness check on the field. An all-zero-but-present
// {} (a literal `push_rules: {}`) carries no actual rule and must read like
// an absent field, same as the Go reader — a policy stored before this field
// existed, and one that sets it to nothing, look identical.
//
// Lives here, not in lib/types/policy.ts, because RunRail below is this
// function's only caller: policy.ts is eager (other exports there reach the
// runs board) and this screen is lazy — bundle-split fix, #181, same pattern
// push-content-card.tsx's isPushContentRequest documents.
export function pushRulesIsSet(s: PushRulesSpec | undefined): boolean {
  return !!s && ((s.deny_paths?.length ?? 0) > 0 || (s.require_review_paths?.length ?? 0) > 0 || (s.max_inspect_pack_mib ?? 0) > 0);
}

export function RunRail({
  governanceProfile,
  savedPolicy,
  cc,
  showModelWarning,
  startup,
  showHoldNote,
  toolRules,
  pushRules,
  unattended,
  launch,
  preflight,
  agentRow,
  modelProvider,
  adoDialog,
}: RunRailProps) {
  // #1328 review round 2, R2-1 — who can reach the Environment step from
  // here, see the noBarrier reason line below.
  const operator = useOperator();
  const access = useViewAccess();
  // Both of finding 1's facts, read rather than asserted: where the model
  // credential lands, and whether this deployment records anything at all.
  // `recordingDisabled` is tri-state — undefined until /healthz answers.
  const cred = preflight.result?.model_credential;
  const gitCredential = preflight.result?.git_credential; // #386, informational — see GitCredentialLine
  const recordingDisabled = useRecordingDisabled();
  const door = useModelAccessDoor();

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
  const refusedProvider = launch.refusedProvider ?? "";
  React.useEffect(() => {
    if (!launch.credentialRefused || autoOpened.current) return;
    autoOpened.current = true;
    if (door.open) return;
    // #543 (§5.8): the door of the provider the refusal names — never the
    // agent or provider selected on screen, which may have moved since the
    // click (#146's ruling). A provider this person has no door for, or a
    // refusal naming no provider (a sign-in renewal that did not complete),
    // opens nothing (resolveDoor's null) and the sentence stands.
    if (refusedProvider) {
      door.openDoor({ for: { provider: refusedProvider }, returnTo: launchRef.current, onSignedIn: () => onLaunchRef.current() });
    }
    // The strip catches up with what the server just said.
    void door.refresh();
  }, [launch.credentialRefused, refusedProvider, door]);

  // A run with no model credential to describe (a shell command — the screen
  // withholds agentRow for one), no model-access line and no warning to raise
  // has no Credentials section at all, rather than a heading over nothing.
  // review follow-up N5: gitCredential contributes only when GitCredentialLine
  // actually renders something for it (state "not_configured") — a `live`
  // gitCredential (nothing to say, see GitCredentialLine above) must not by
  // itself open an empty heading over a shell run with nothing else to show.
  const showCredentials =
    hasProviderCandidates || showModelWarning || !!cred || !!agentRow || gitCredentialSpeaks(gitCredential);
  // With no provider connected and nothing resolved, "Resolved at launch."
  // and the Preflight hint must not sit directly under "No model provider is
  // connected. This run launches; its first model call fails." Nothing
  // resolves at launch when there is nothing to resolve. A resolved credential
  // still states itself — that sentence is read off the verdict, not guessed.
  const showCredentialFacts = !!cred || (!!agentRow && !showModelWarning);
  // #181 review finding 6 — pushRulesIsSet(pushRules) alone is true for a
  // policy that sets ONLY max_inspect_pack_mib (no deny_paths/
  // require_review_paths at all): there is nothing to say about PATHS in
  // that case, and "0 paths denied · 0 paths held for review" reads as a
  // real (empty) rule set rather than "no path rule". The section stays
  // hidden entirely rather than rendering that sentence.
  const pushDeniedCount = pushRules?.deny_paths?.length ?? 0;
  const pushReviewCount = pushRules?.require_review_paths?.length ?? 0;
  const showPushRules = pushRulesIsSet(pushRules) && (pushDeniedCount > 0 || pushReviewCount > 0);
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
          {!hasProviderCandidates && showModelWarning && (
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
            <CredentialFacts cred={cred} preflightRun={!!preflight.result} />
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

        {/* #181 — push_rules counts, the same "policy has this section or it
            doesn't" shape Tool rules above uses. showPushRules mirrors the Go
            PushRulesSpec.IsSet() reader AND requires at least one actual
            path rule (review finding 6) — a stored `{}`, or a spec that sets
            only max_inspect_pack_mib, reads as no section rather than "0
            paths denied · 0 paths held for review". The unattended note is
            gated on require_review_paths alone: an unattended run refuses a
            REVIEW match outright (push_rules.go), but a deny_paths match is
            refused identically whether the run is attended or not, so the
            note would be true of a section with no review rule in it. */}
        {showPushRules && (
          <RailSection title={PUSH.RAIL_TITLE}>
            <p className="text-xs text-muted-foreground">{PUSH.RAIL_BODY(pushDeniedCount, pushReviewCount)}</p>
            {unattended && pushReviewCount > 0 && (
              <p className="mt-1 text-xs text-muted-foreground">{PUSH.RAIL_UNATTENDED}</p>
            )}
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

      {/* Preflight lives on the Policy panel, next to the document it checks —
          one button, not two competing ones. Its result stays here, beside
          Launch, because "what would be clamped" is the last thing read before
          committing. #125: a 2xx launch (warnings or not) navigates straight to
          the run in the same tick, so there is no longer a held state for this
          button to become — any advisory warnings render on the run page
          instead (run-detail/launch-warnings-note.tsx). */}
      <div className="mt-4 flex gap-2">
        <Button
          ref={launchRef}
          type="button"
          className="flex-1"
          disabled={launch.disabled || !!launch.problem || !!launch.workspaceUnavailable || !!launch.noBarrier}
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
      {/* #214 — the one control that genuinely cannot work says so beside
          itself, not in a tooltip, with a route to the step that fixes it.
          A separate line from `problem` above (never both: the Barrier
          section's own TierPicker card already gives the detailed reason;
          this is Launch's own, short pointer to the fix).

          #1328 review round 2, R2-1 — the CTA itself renders only for a
          caller who can actually reach the Environment step: an operator
          (already resolves NO_BARRIER.ADMIN_ROUTE directly, whichever view
          they're in) or a session-user (an SSO admin in the User view, whom
          ViewGate's own "to-admin" interstitial asks before switching — see
          NO_BARRIER's doc comment). Everyone else reads the reason alone;
          there is nothing behind that route they may open. */}
      {launch.noBarrier && !launch.inFlight && (
        <p className="mt-2 text-center text-xs text-muted-foreground">
          {NO_BARRIER.LAUNCH_REASON}
          {(operator || access === "session-user") && (
            <>
              {" "}
              <Link to={NO_BARRIER.ADMIN_ROUTE} className="font-medium text-info hover:underline">
                {NO_BARRIER.CTA}
              </Link>
              .
            </>
          )}
        </p>
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
