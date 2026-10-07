/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Link } from "react-router-dom";
import type { PushRulesSpec, SCMAccess, SetupModelProvider } from "../../../lib/types";
import { PUSH } from "../../wardyn/copy/push";
import { AutonomyChip, Chip } from "../../wardyn/primitives";
import { CC_META } from "../../wardyn/cc-meta";
import { AUTONOMY_RAIL, autonomyBoundSentence, GOVERNANCE as GOV, MEMBER } from "../../../lib/governance-copy";
import { ADO } from "../../../lib/ado-entra-copy";
import type { SCMAccessPAT } from "../../../lib/types/ado-pat";
import { RAIL_RECORDING_ON, RECORDING_DISABLED_TITLE, RUN } from "../../wardyn/copy";
import { RailSection } from "./new-run-primitives";
import { PolicyRemedy } from "../../wardyn/policy-remedy";
import { CredentialFacts, ModelProviderSection } from "./new-run-rail-credentials";
import { RAIL_MODEL_ACCESS } from "../../wardyn/model-access-copy";
import type { RunRailProps } from "./new-run-rail-types";

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
// Lives here, not in lib/types/policy.ts, because RunRailSummary below is this
// function's only caller: policy.ts is eager (other exports there reach the
// runs board) and this screen is lazy — bundle-split fix, #181, same pattern
// push-content-card.tsx's isPushContentRequest documents.
export function pushRulesIsSet(s: PushRulesSpec | undefined): boolean {
  return (
    !!s &&
    ((s.deny_paths?.length ?? 0) > 0 ||
      (s.require_review_paths?.length ?? 0) > 0 ||
      (s.max_inspect_pack_mib ?? 0) > 0 ||
      (s.max_file_size_mib ?? 0) > 0)
  );
}

export function RunRailSummary({
  governanceProfile, governanceContact, savedPolicy, cc, showModelWarning,
  modelBlocked, startup, showHoldNote, toolRules, pushRules, unattended,
  preflight, agentRow, modelProvider, recordingDisabled, onProviderSignIn,
}: Pick<RunRailProps,
  "governanceProfile" | "governanceContact" | "savedPolicy" | "cc" | "showModelWarning" |
  "modelBlocked" | "startup" | "showHoldNote" | "toolRules" | "pushRules" | "unattended" |
  "preflight" | "agentRow" | "modelProvider"
> & {
  recordingDisabled: boolean | undefined;
  onProviderSignIn: (provider: SetupModelProvider) => void;
}) {
  const cred = preflight.result?.model_credential;
  const gitCredential = preflight.result?.git_credential; // #386, informational — see GitCredentialLine
  // #542 — a provider block with at least one candidate for the picked agent,
  // OR a gate to name (R5c — the rail-gap packet), supersedes
  // CredentialFacts/showModelWarning entirely; with neither (no block, or none
  // serving this agent — R9) that legacy path is unchanged below.
  const hasProviderCandidates = !!modelProvider && (modelProvider.candidates.length > 0 || !!modelProvider.gate);
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
  const showCredentialFacts = !!cred || (!!agentRow && !showModelWarning && !modelBlocked);
  // #181 review finding 6 — pushRulesIsSet(pushRules) alone is true for a
  // policy that sets ONLY max_inspect_pack_mib (no deny_paths/
  // require_review_paths at all): there is nothing to say about PATHS in
  // that case, and "0 paths denied · 0 paths held for review" reads as a
  // real (empty) rule set rather than "no path rule". The section stays
  // hidden entirely rather than rendering that sentence.
  const pushDeniedCount = pushRules?.deny_paths?.length ?? 0;
  const pushReviewCount = pushRules?.require_review_paths?.length ?? 0;
  const showPushRules = pushRulesIsSet(pushRules) && (pushDeniedCount > 0 || pushReviewCount > 0);
  // Below lg the rail sits under the form at full width, so its sections
  // read across instead of stacking into a very tall column.
  return (
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
              <PolicyRemedy policy={governanceContact} className="mt-1 block" />
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
        {!hasProviderCandidates && showModelWarning && !modelBlocked && (
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
  );
}
