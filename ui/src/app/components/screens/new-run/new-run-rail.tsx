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
import { RAIL_CREDENTIAL, RECORDING_DISABLED_TITLE, RUN } from "../../wardyn/copy";
import { useRecordingDisabled } from "../../../lib/hooks/use-recording-disabled";
import { RailSection } from "./new-run-primitives";

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
   * The picked agent's /setup/status roster row, when the screen has one. It
   * carries the DEFAULT-path answer to "where does this run's model credential
   * live" — credential_residency, graded server-side from the lanes that
   * actually resolve. Undefined for an agent the status read did not name, or
   * before that read lands: the absent-row doctrine again, and the Credentials
   * section falls back to "Resolved at launch." rather than to the sentence the
   * rail used to assert unconditionally.
   */
  agentRow?: SetupHarnessTool;
}

// modelCredentialOf is the rail's ONE precedence rule: a preflight the operator
// actually ran describes the body they are about to launch, so it wins; the
// status row is what everyone else reads, since Preflight is a manual button
// nothing fires by default and answers 422 for a per_user member who has not
// signed in yet.
//
// Returns undefined when NEITHER source graded one — never a default. The old
// copy's whole defect was that it had a default.
function modelCredentialOf(
  preflight: PreflightResult | null,
  agentRow?: SetupHarnessTool,
): ModelCredential | undefined {
  if (preflight?.model_credential) return preflight.model_credential;
  if (!agentRow?.credential_residency) return undefined;
  return {
    residency: agentRow.credential_residency,
    mechanism: agentRow.mechanism,
    credential_source: agentRow.credential_source,
    staged_placeholder: agentRow.staged_placeholder,
  };
}

// CredentialFacts renders where the MODEL credential lands, and nothing wider:
// every sentence names the model credential explicitly, because "Credentials" as
// a heading over "never written into the sandbox" was a universal claim only the
// model credential ever supported — env_secret and ssh_key grants are resident
// by design (threatmodel/THREAT-MODEL.md's resident-secret table), which is what
// the policy line at the bottom is for.
function CredentialFacts({
  cred,
  savedPolicy,
}: {
  cred?: ModelCredential;
  savedPolicy?: { name: string; spec: RunPolicySpec };
}) {
  // env_secret / ssh_key are delivered INTO the sandbox whatever the model
  // credential does. Only for a saved policy, whose spec is the one the rail
  // actually holds — an inline spec is not a prop here, and a line about grants
  // the rail cannot see would be the same kind of unconditional claim.
  const residentGrants = (savedPolicy?.spec.eligible_grants ?? []).some(
    (g) => g.kind === "env_secret" || g.kind === "ssh_key",
  );
  return (
    <>
      <p className="text-xs leading-relaxed text-muted-foreground">
        {credentialSentence(cred)}
      </p>
      {cred?.residency === "sandbox" && cred.mechanism !== "anthropic_subscription" && (
        <div className="mt-1.5">
          <Chip tone="neutral">
            {cred.credential_source === "per_user"
              ? RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER
              : RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_SHARED}
          </Chip>
        </div>
      )}
      {residentGrants && (
        <p className="mt-1.5 text-xs leading-relaxed text-muted-foreground">
          {RAIL_CREDENTIAL.POLICY_GRANTS_SECRETS}
        </p>
      )}
    </>
  );
}

// credentialSentence maps a graded residency to the one sentence that is true of
// it. The "unknown" arm is also the arm an absent grade takes, so the proxy
// sentence is unreachable from an absence — the defect this lane exists for.
function credentialSentence(cred?: ModelCredential): string {
  switch (cred?.residency) {
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
  const cred = modelCredentialOf(preflight.result, agentRow);
  const recordingDisabled = useRecordingDisabled();
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

        <RailSection title="Credentials">
          {showModelWarning && (
            <p className="mb-1.5 rounded-md border border-warning/30 bg-warning-subtle px-2 py-1.5 text-xs text-foreground">
              No model provider is connected. This run launches; its first model call fails.{" "}
              {/* Rulebook §9: the action that fills the gap rides next to the
                  need, not only in a footer. Links are --info, never teal. */}
              <Link to="/settings" className="font-medium text-info hover:underline">
                Connect →
              </Link>
            </p>
          )}
          <CredentialFacts cred={cred} savedPolicy={savedPolicy} />
        </RailSection>

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

        <RailSection title="Recording">
          {/* A stock Helm install leaves persistence.enabled=false, so this
              promise was false out of the box — and wrong in both dangerous
              directions at once. The shared hook is the same /healthz read the
              Recordings library and the run cockpit make. */}
          <p className="text-xs text-muted-foreground">
            {recordingDisabled
              ? RECORDING_DISABLED_TITLE
              : "Every keystroke and every outbound connection."}
          </p>
        </RailSection>
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
