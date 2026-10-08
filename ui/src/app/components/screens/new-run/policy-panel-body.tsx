/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Policy panel (#1922), first slice: the barrier, the three policy modes
// and the policy editor New Run already had, moved here unchanged. The shared
// read-and-edit policy document replaces this body in a later slice; the props
// are that seam.
//
// `inline_policy` on POST /runs is the identical struct a saved policy stores,
// validated by the same validator, so this screen and /policies author it
// through ONE component (wardyn/policy-panel.tsx). What the JSON cannot know
// (the Workspace panel's mounts/repos, the grant lanes' grants and hosts) is
// unioned back in by mergeRunSelections and NAMED on screen, never merged
// behind the operator's back.
import type * as React from "react";
import { Link } from "react-router-dom";
import { CC_ORDER as ORDERED_CLASSES } from "../../../lib/types";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { CC_META } from "../../wardyn/cc-meta";
import { RUN } from "../../wardyn/copy";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { PolicyPanel } from "../../wardyn/policy-panel";
import { TierPicker, allowedFromFloor } from "../../wardyn/tier-picker";
import { TIER_PICKER } from "../../../lib/tier-picker-copy";
import { ISSUE_LINE_ID, ISSUE_TARGET, type LaunchIssue } from "./new-run-launch-gates";
import { barrierRequirementReason } from "./policy-lane";
import { previewSpec } from "./use-default-policy";
import type { useNewRunController } from "./use-new-run-controller";

type Controller = ReturnType<typeof useNewRunController>;

const SAVED_GONE_ID = "nr-saved-policy-gone";

export interface PolicyPanelBodyProps {
  c: Pick<
    Controller,
    | "state" | "patch" | "policy" | "cc" | "setCcTouched" | "probeSettled" | "availableClasses" | "vaultReason"
    | "specText" | "onSpecChange" | "preflight" | "preflighting" | "policyMode" | "gates" | "isInteractive"
    | "adoCeiling" | "modelProviders" | "defaultRead" | "defaultPolicy" | "governanceProfile" | "retryDefault"
    | "onPolicyModeChange" | "onPickPolicy" | "savedPolicies" | "operator" | "hasAdditions" | "added"
  >;
  /** The Policy panel is the one on screen. */
  active: boolean;
  /** Every reason Launch is held, and the one the line above Launch names. */
  issues: LaunchIssue[];
  shown: LaunchIssue | null;
  /** This screen's own links ask the unsaved guard before they leave. */
  guardLink: (to: string) => (e: React.MouseEvent) => void;
}

export function PolicyPanelBody({ c, active, issues, shown, guardLink }: PolicyPanelBodyProps) {
  const {
    state, patch, policy, cc, setCcTouched, probeSettled, availableClasses, vaultReason,
    specText, onSpecChange, preflight, preflighting, policyMode, gates, isInteractive,
    adoCeiling, modelProviders, defaultRead, defaultPolicy, governanceProfile, retryDefault,
    onPolicyModeChange, onPickPolicy, savedPolicies, operator, hasAdditions, added,
  } = c;
  const savedIssue = issues.find((i) => i.focus === ISSUE_TARGET.SAVED_POLICY);
  // Printed beside the picker while Policy is on screen, above Launch from
  // every other panel: never both.
  const savedInline = !!savedIssue?.inline && active;
  return (
    <div className="space-y-4">
      {/* #214 — the Barrier control ahead of the policy it floors: it decides
          whether a run is confined at all. */}
      <div id={ISSUE_TARGET.BARRIER} tabIndex={-1}>
        {/* #1200 — the shared TierPicker: only what THIS run can actually
            use (installed ∧ at-or-above the active floor, folding in the
            governance ceiling only where the server would clamp to it —
            govFloorApplies). A tier the floor forbids or the host can't
            build is DROPPED, never shown disabled (the global picker
            rule); ONE qualifying tier collapses to the decided row, NONE
            shows the T-9 requirement card. requirementNote below names
            the active floor whenever one is set — every fresh form has
            one, the default spec's CC1 — so TierPicker's own
            REQUIREMENT_TITLE shows only when no floor is authored; the
            no-runner (#214) reason is the rail's noBarrier line.
            Review P2-1/P2-3: decidedLine/pickOneNote override
            TierPicker's defaults, which assume a governance floor and a
            browser-persisted pick — neither true here. */}
        <TierPicker
          // P2-7: an UNKNOWN probe (qualifying: null) must not offer
          // a tier the active floor already forbids — it falls back
          // to the floor's own allowed set, not the unfiltered
          // ORDERED_CLASSES, and only to that when there is no floor
          // either.
          tiers={policy.qualifying ?? allowedFromFloor(policy.effectiveFloor) ?? ORDERED_CLASSES}
          selected={cc}
          onSelect={(id) => {
            setCcTouched(true);
            patch({ confinementClass: id });
          }}
          decidedLine={policy.governanceBinding ? undefined : () => RUN.BARRIER_ONLY_QUALIFIER}
          pickOneNote={TIER_PICKER.PICK_ONE_PER_RUN}
          readiness={!probeSettled ? "checking" : availableClasses ? "ready" : "unverified"}
          requirementNote={
            // A host with no barrier at all (noBarrierOnHost) is not "the
            // floor needs a tier this host lacks": every fresh form carries
            // the default CC1 floor, so that line would name Fence as the
            // missing piece. The picker's own no-runner title stands, and
            // the rail beside Launch gives the reason.
            !policy.noBarrierOnHost && policy.qualifying && policy.qualifying.length === 0 && policy.effectiveFloor
              ? (policy.governanceBinding ? TIER_PICKER.GOVERNANCE_REQUIREMENT_LINE : TIER_PICKER.REQUIREMENT_LINE)(
                  CC_META[policy.effectiveFloor].label,
                  barrierRequirementReason(policy.effectiveFloor, policy.unavailable, policy.belowFloor, vaultReason),
                )
              : undefined
          }
        />
        {/* Unknown never blocks launch: an untouched pick sends no
            confinement_class (ccTouched), so the server decides. */}
        {probeSettled && !availableClasses && (
          <p className="mt-2 text-xs text-muted-foreground">{RUN.BARRIER_UNKNOWN}</p>
        )}
      </div>

      <div id={ISSUE_TARGET.POLICY_MODE} tabIndex={-1}>
        <PolicyPanel
          instance="run"
          value={specText}
          onChange={onSpecChange}
          onPreflight={preflight}
          preflightBusy={preflighting}
          preflightDisabled={(policyMode === "saved" && !state.selectedPolicyId) || gates.referenceWorkspaceBlocked}
          interactive={isInteractive}
          adoCeiling={adoCeiling}
          modelProviders={modelProviders}
          policyMode={{
            mode: policyMode,
            defaultPolicy: {
              status: defaultRead.status,
              spec: defaultPolicy && previewSpec(defaultPolicy),
              profileName: governanceProfile,
              onRetry: retryDefault,
            },
            // Printed once, beside Check again; Check again and Launch name it
            // as their description (policy-panel.tsx's POLICY_HOLD_ID).
            hold: gates.referenceWorkspaceProblem,
            onModeChange: onPolicyModeChange,
            picker: (
              <div className="space-y-2">
                <Select value={state.selectedPolicyId ?? ""} onValueChange={onPickPolicy}>
                  <SelectTrigger
                    id={ISSUE_TARGET.SAVED_POLICY}
                    aria-label="Saved policy"
                    aria-invalid={!!savedIssue || undefined}
                    aria-describedby={savedInline ? SAVED_GONE_ID : savedIssue && shown === savedIssue ? ISSUE_LINE_ID : undefined}
                  >
                    <SelectValue placeholder="Pick a policy" />
                  </SelectTrigger>
                  <SelectContent>
                    {savedPolicies.map((p) => (
                      <SelectItem key={p.id} value={p.id}>
                        {p.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {savedIssue && savedInline && (
                  <p id={SAVED_GONE_ID} className="text-xs text-warning">
                    {savedIssue.text}
                  </p>
                )}
                {/* Rulebook §9: an empty picker carries the action that
                    fills it. M-1b: Policies is Admin view only, so the
                    door renders only for the tier that authors them. */}
                {savedPolicies.length === 0 && (
                  <p className="text-xs text-muted-foreground">
                    No saved policies yet
                    {operator && (
                      <>
                        {" "}·{" "}
                        <Link to="/admin/policies" className="font-medium text-info hover:underline" onClick={guardLink("/admin/policies")}>
                          New policy →
                        </Link>
                      </>
                    )}
                  </p>
                )}
              </div>
            ),
          }}
        />
      </div>

      {/* C5: named, not left to the barrier above silently winning. */}
      {policyMode === "custom" && policy.unparseableFloor && (
        <p className="text-xs text-warning">{AGENTS.FLOOR_UNPARSEABLE(policy.unparseableFloor)}</p>
      )}

      {/* What buildSpec unions in AFTER the parse, named out loud. A
          policy the operator did not write is one they cannot be held
          to — and these are exactly the entries the document itself
          cannot know: the Workspace panel's attachments, the grant
          lanes' grants, and the hosts those grants must reach (an
          api_key grant whose host is not on the allowlist
          authenticates nothing, allow_all_egress included). Saved-policy runs launch by REFERENCE, so nothing
          is merged into a stored spec. */}
      {policyMode === "custom" && hasAdditions && added && (
        <div className="rounded-lg border border-border bg-surface-2 p-3" data-testid="run-spec-additions">
          <p className="text-xs text-muted-foreground">Added for this run&apos;s selections:</p>
          <div className="mt-1.5 space-y-1">
            {added.hosts.map((h) => (
              <div key={h} className="flex flex-wrap items-center gap-1.5">
                <Mono className="text-xs text-foreground">{h}</Mono>
                <Chip tone="neutral">allowed_domains</Chip>
              </div>
            ))}
            {added.grants.map((g, i) => (
              <div key={`${g.kind}-${i}`} className="flex flex-wrap items-center gap-1.5">
                <Mono className="text-xs text-foreground">{String(g.kind)}</Mono>
                <Chip tone="neutral">eligible_grants</Chip>
              </div>
            ))}
            {added.mounts.map((m) => (
              <div key={m.target} className="flex flex-wrap items-center gap-1.5">
                <Mono className="text-xs text-foreground">{m.target}</Mono>
                <Chip tone="neutral">workspace_mounts</Chip>
              </div>
            ))}
            {added.repos.map((r) => (
              <div key={r.repo} className="flex flex-wrap items-center gap-1.5">
                <Mono className="text-xs text-foreground">{r.repo}</Mono>
                <Chip tone="neutral">workspace_repos</Chip>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
