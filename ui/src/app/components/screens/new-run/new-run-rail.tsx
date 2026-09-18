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
  SetupHarnessTool,
} from "../../../lib/types";
import { Button } from "../../ui/button";
import { Chip, ConfinementChip, RiskBadge } from "../../wardyn/primitives";
import { CC_META } from "../../wardyn/cc-meta";
import { GOVERNANCE as GOV, MEMBER } from "../../../lib/governance-copy";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { RAIL_CREDENTIAL, RAIL_RECORDING_ON, RECORDING_DISABLED_TITLE, RUN } from "../../wardyn/copy";
import { useRecordingDisabled } from "../../../lib/hooks/use-recording-disabled";
import { RailSection } from "./new-run-primitives";
import { MODEL_ACCESS_AGENT } from "../../../lib/model-access";
import { absoluteTime, relativeTime } from "../../../lib/format";
import { RAIL_MODEL_ACCESS } from "../../wardyn/model-access-copy";
import {
  useClaimModelAccessDoor,
  useModelAccessDoor,
  type ModelAccessDoorHandle,
} from "../../wardyn/model-access-context";

interface RunRailProps {
  /**
   * The governance profile bounding THIS caller, from GET /policies/default's
   * governance_profile_name. Undefined for a caller with no assignment — the
   * absent-row doctrine, and the section below simply does not render, so an
   * unassigned member's rail is byte-for-byte what it was.
   */
  governanceProfile?: string;
  /** The stored policy this run launches by reference, when there is one. */
  savedPolicy?: { name: string; spec: RunPolicySpec };
  /** The barrier the run REQUESTS (a separate wire field from the spec floor). */
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
    onLaunch: () => void;
    /** useDeferredBusy: disabled the instant it fires. */
    disabled: boolean;
    /** useDeferredBusy: the spinner arrives ~200ms later. */
    spinning: boolean;
    inFlight: boolean;
    /** Why Launch cannot be pressed — a disabled button that won't say is a dead end. */
    problem: string | null;
    error: string | null;
    /** The 201's advisory `warnings[]`, once Launch has actually fired
     *  (§5c.8) — rendered here, inline, instead of a toast. */
    warnings: string[];
    /** Set once a run launched WITH warnings: the screen stays put and this
     *  replaces Launch, so the member opens the run when they have read them.
     *  Null on every other state. A timed redirect used to do this and raced
     *  every other navigation off the screen. */
    onOpenRun: (() => void) | null;
  };
  preflight: { error: string | null; result: PreflightResult | null };
  /**
   * The picked agent's /setup/status roster row — WITHHELD by the screen for a
   * run that makes no model call (a shell command), so its absence is also how
   * this rail knows there is no model credential to describe.
   *
   * Its `credential_residency` is published for ONE row shape only (an enabled
   * per_user + bedrock_sso row) and is the only thing read off it here. The
   * row's `mechanism` is the DECLARED lane and is NEVER read: under a `shared`
   * row that lane is satisfied by a chain that fell through to a different,
   * resident one, and in legacy mode the field is empty — keying a sentence on it
   * rendered "AWS credentials sign inside the sandbox" over a Claude sign-in.
   */
  agentRow?: SetupHarnessTool;
}

// CredentialFacts states where the MODEL credential lands, and nothing wider —
// "Credentials" as a heading over "never written into the sandbox" was a
// universal claim only the model credential ever supported.
//
// THE PRECEDENCE, and why it is only two rungs. A CURRENT preflight verdict
// describes the exact body about to be launched, resolved lane and all, so it
// wins and everything below is read off IT. Otherwise the only claim available
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
  /** Whether a CURRENT preflight verdict is on screen (U-4). */
  preflightRun: boolean;
}) {
  if (cred) {
    // Keyed on the RESOLVED mechanism, never on the row's declared one.
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
      {/* U-4: …and the way to find out, ONLY while there is nothing to find it
          in. A current verdict that carries no `model_credential` — always so
          against a 0.7.4 daemon, and on 0.7.5 whenever the roster read failed or
          there is no store — left this hint beside the result of pressing it: a
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

// credentialSentence maps a RESOLVED grade to the one sentence true of it.
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

// ModelAccessLine — Finding 1: the rail states WHO (this launcher) needs to
// sign in, a second and independent fact from showModelWarning above (a
// DEPLOYMENT with no model path at all). RunRail withholds it entirely unless
// the selected agent is the one model_access grades AND the door needs
// attention (showModelAccess) — a codex row or a live session renders nothing.
//
// `// ponytail:` this reads useModelAccessDoor() itself rather than taking the
// door as a prop threaded from new-run-screen.tsx: that screen is at its
// 1000-line file-size gate and gets a ZERO-line diff (the context exists
// exactly so a nested surface can reach the door with no prop-drilling).
function ModelAccessLine({ door, onSignIn }: { door: ModelAccessDoorHandle; onSignIn: () => void }) {
  const when = door.deadline ? relativeTime(door.deadline) : "";
  let sentence = "";
  let action = "";
  let title = "";
  // `expiring` is a STATE, not an alarm (round-1 UX S3, round-2 S14) — muted
  // text, not the warning tint the other three states use.
  let warning = true;
  // The server's action ONLY when it carries what the sentence and button
  // cannot — the pin-contradicted account/role pair — never the button's own
  // label repeated as prose (S1).
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
      // No separate action line — the deadline is IN the sentence (S1 / W0-mock
      // ruling 1) — EXCEPT against a daemon that sends no `deadline` (review-1
      // S3): an older daemon's `expiring` state would otherwise render NOTHING
      // at all here while the rail still CLAIMS the door — zero sign-in
      // controls on /runs/new. Mirrors the strip's own fallback.
      sentence = when ? RAIL_MODEL_ACCESS.EXPIRING(when) : "";
      action = when ? "" : door.action;
      break;
    case "shared_expired":
      // The one credential every run rides. Its ADMIN reads their own repair
      // sentence, never the member's "ask them" line about themselves
      // (review-1 S2) — everybody else keeps the server's instruction.
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
      {/* Two SEPARATE text nodes (mirrors model-access-banner.tsx's
          modelAccessStripCopy rendering) — the server's action, when it
          renders, is a second fact beside ours, never appended into the same
          sentence. */}
      {sentence && <span>{sentence}</span>}
      {action && <span> {action}</span>}
      {/* The rail's OWN sign-in, under a DISTINCT accessible name from the
          strip's/Getting Started's "Sign in to AWS" (U-13's actual rule is two
          controls with distinct names, not one hidden) — and hidden while the
          door dialog is open, so there is never a live control pointing at a
          dialog that is already on screen. Gated on door.actionable, not just
          needsAttention: a non-operator's shared_expired has nothing this
          viewer can repair (round-1 UX S13). */}
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
}: RunRailProps) {
  // Both of finding 1's facts, read rather than asserted: where the model
  // credential lands, and whether this deployment records anything at all.
  // `recordingDisabled` is TRI-STATE — undefined until /healthz answers.
  const cred = preflight.result?.model_credential;
  const recordingDisabled = useRecordingDisabled();
  // Finding 1: model_access grades the claude-code row alone, so a shell
  // command or a different agent (codex) never reads this line whatever the
  // door says.
  const door = useModelAccessDoor();
  const showModelAccess = agentRow?.id === MODEL_ACCESS_AGENT && door.needsAttention;
  // Door ownership (round-2 UX B1/S2): the rail claims it for exactly as long
  // as it renders its own sign-in control, so the shell strip drops its
  // button here — no New Run exception — and keeps its sentence.
  useClaimModelAccessDoor(showModelAccess && door.actionable);

  // Focus returns to Launch, not to #main-content (which would drop the
  // member at the top of the form they were mid-way through), when THIS
  // rail's own control opened the door. The DOOR owns the return target
  // (review-1 S1): the rail's own sign-in control unmounts the moment the
  // state it described clears (a completed sign-in), so by the time the
  // dialog's onCloseAutoFocus runs, document.activeElement — what a bare
  // openDoor() would have captured — is a DETACHED node and focusOpener()
  // fails, falling through to #main-content; a separate effect here racing
  // Radix's own FocusScope exit trap cannot reliably win either. Passing
  // Launch explicitly as `returnTo` makes it the captured opener directly.
  const launchRef = React.useRef<HTMLButtonElement>(null);

  // A run with no model credential to describe (a shell command — the screen
  // withholds agentRow for one), no model-access line and no warning to raise
  // has no Credentials section at all, rather than a heading over nothing.
  const showCredentials = showModelWarning || !!cred || !!agentRow || showModelAccess;
  // U-5: with NO provider connected and nothing resolved, "Resolved at launch."
  // and the Preflight hint sat directly under "No model provider is connected.
  // This run launches; its first model call fails." Nothing resolves at launch
  // when there is nothing to resolve. A RESOLVED credential still states itself
  // — that sentence is read off the verdict, not guessed.
  const showCredentialFacts = !!cred || (!!agentRow && !showModelWarning);
  return (
    // F2-F7/F3-F1: a sticky box is clamped by its containing block — with
    // ceiling + tool rules + 3 warnings (member/warnings path) the rail's
    // real content runs ~700-730px, below the fold at 1280x650 with no way
    // to reach Launch. Bounded to the viewport with its own scroll.
    //
    // 100vh - 5rem, not -3rem: the sticky container is app-shell.tsx's
    // <main> (its own overflow-y:auto scroller), which starts BELOW the
    // h-14 (3.5rem/56px) header — sticky's `top-6` (1.5rem/24px) offset is
    // relative to THAT scroller, not the viewport, so the rail's stuck
    // position sits at 3.5rem+1.5rem = 5rem from the viewport top, not 1.5rem.
    <aside className="h-fit rounded-xl border border-border bg-card p-4 lg:sticky lg:top-6 lg:max-h-[calc(100vh-5rem)] lg:overflow-y-auto">
      <p className="mb-3 text-sm font-semibold text-foreground">What this run can do</p>

      {/* Below lg the rail sits UNDER the form at full width, so its sections
          read across instead of stacking into a very tall column. */}
      <div className="grid gap-x-6 gap-y-3 sm:grid-cols-2 md:grid-cols-4 lg:grid-cols-1">
        {/* FIRST, above Policy, because it bounds everything under it — a
            member's own spec AND a saved policy they pick are both clamped to
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

        {showCredentials && (
        <RailSection title="Credentials">
          {/* Finding 1, above CredentialFacts: a per-PERSON fact ("do I have a
              sign-in at all"), independent of showModelWarning below (a
              DEPLOYMENT fact — some model path exists at all). */}
          {showModelAccess && (
            <ModelAccessLine door={door} onSignIn={() => door.openDoor(launchRef.current)} />
          )}
          {showModelWarning && (
            <p className="mb-1.5 rounded-md border border-warning/30 bg-warning-subtle px-2 py-1.5 text-xs text-foreground">
              {RAIL_MODEL_ACCESS.NO_PROVIDER}{" "}
              {/* Rulebook §9: the action that fills the gap rides next to the
                  need, not only in a footer. Links are --info, never teal. */}
              <Link to="/settings" className="font-medium text-info hover:underline">
                {RAIL_MODEL_ACCESS.NO_PROVIDER_CTA}
              </Link>
            </p>
          )}
          {showCredentialFacts && (
            <CredentialFacts cred={cred} agentRow={agentRow} preflightRun={!!preflight.result} />
          )}
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

        {/* One line, and it NAMES the tools: "3 rules" alone would say nothing
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

            UNKNOWN renders nothing: a promise this specific may not be made
            from a /healthz read that has not landed, failed, or carried no
            recording component at all — and U-15: the whole SECTION goes with
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
        <p className="mt-3 flex items-start gap-1.5 text-xs text-danger">
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
          {launch.error}
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

      {/* Preflight moved ONTO the Policy panel, next to the document it checks —
          one button, not two competing ones. Its result stays here, beside
          Launch, because "what would be clamped" is the last thing read before
          committing. */}
      <div className="mt-4 flex gap-2">
        {launch.onOpenRun ? (
          // The run IS launched — Launch has nothing left to do, and the one
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
            onClick={launch.onLaunch}
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
      {/* A disabled button that doesn't say why is a dead end. This screen had
          NO client-side validation at all before — an empty form launched, and
          the server's rejection arrived after the fact. */}
      {launch.problem && !launch.inFlight && (
        <p className="mt-2 text-center text-xs text-muted-foreground">{launch.problem}</p>
      )}

      {preflight.error && (
        <p className="mt-3 flex items-start gap-1.5 text-xs text-danger">
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
          {preflight.error}
        </p>
      )}
      {/* UNFRAMED: a bordered box inside the rail card is a card in a card
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
    </aside>
  );
}
