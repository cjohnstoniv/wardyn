/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New run — ONE page, two columns, with a live rail that answers "what can this
// run actually do?" while you build it. Replaces a five-step modal wizard whose
// Review screen was the first place the consequences of your choices appeared.
//
// A RE-LAYOUT, not a re-model: buildSpec()/impliedEgressHosts() are reused
// verbatim, so the launch payload is exactly what the wizard produced — the
// governance contract is the tested part; this is about when the operator SEES
// it, not what it is.
//
// The Confinement and Network cards are GONE — `inline_policy` on POST /runs is
// the identical struct a saved policy stores, validated by the same validator,
// so this screen and /policies now author it through ONE component
// (wardyn/policy-panel.tsx). What the JSON
// cannot know (the Workspace card's mounts/repos, the grant lanes' grants and
// hosts) is unioned back in by mergeRunSelections and NAMED on screen, never
// merged behind the operator's back.
import { ArrowLeft } from "lucide-react";
import { toast } from "sonner";
import { CC_ORDER as ORDERED_CLASSES, type Workspace } from "../../../lib/types";
import { Link } from "react-router-dom";
import { SectionCard } from "./new-run-primitives";
import { ADOAccessSummary } from "../../wardyn/ado-access-summary";
import { AdoLaunchNote, AdoRunTokenLine } from "../../wardyn/ado-run-token-line";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Field } from "../../wardyn/form-primitives";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { CC_META } from "../../wardyn/cc-meta";
import { RUN } from "../../wardyn/copy";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { PolicyPanel } from "../../wardyn/policy-panel";
import { TierPicker, allowedFromFloor } from "../../wardyn/tier-picker";
import { TIER_PICKER } from "../../../lib/tier-picker-copy";
import { AddWorkspaceDialog } from "../add-workspace-dialog";
import { WorkspaceCard } from "./workspace-card";
import { barrierRequirementReason } from "./policy-lane";
import { WhatToRunStep } from "./step-bodies";
import { previewSpec } from "./use-default-policy";
import { NewRunLaunchPanel } from "./new-run-launch-panel";
import { useNewRunController } from "./use-new-run-controller";

export function NewRunScreen() {
  const {
    navigate, prefill, state, setTitleUserEdited,
    patch, knownTitles, isAgent, isInteractive,
    agentName, harnesses, workspaces, caps,
    modelProviders, setAddWsOpen, userDrive, driveDeniedBy,
    driveUnavailable, policy, cc, setCcTouched,
    probeSettled, availableClasses, vaultReason, specText,
    onSpecChange, preflight, preflighting, policyMode,
    gates, adoCeiling, defaultRead, defaultPolicy,
    governanceProfile, retryDefault,
    onPolicyModeChange, onPickPolicy, savedPolicies,
    adoAccess, operator, hasAdditions, added,
    governanceContact, llmReady, pushRules, unattended,
    launch, launchDisabled, launchSpinning, launching,
    policiesLoaded, pin, error, errorSeq,
    errorPolicy, credentialRefused, refusedProvider, currentBody, draftRevision,
    preflightRefusal, invalidateChecks, preflightIsCurrent, preflightFresh, preflightBlock,
    preflightNotChecked, preflightError, preflightErrorSeq, preflightResult,
    providerCandidates, providerAccess, onModelProviderChange, providerChangeNote,
    providerGateState, addWsOpen, reloadWorkspaces,
  } = useNewRunController();

  return (
    <div className="mx-auto w-full max-w-[1200px] px-6 py-6">
      <div className="mb-6 flex items-center gap-3">
        <Button variant="ghost" size="sm" onClick={() => navigate("/runs")}>
          <ArrowLeft className="size-4" /> Runs
        </Button>
        <h1 className="text-foreground">New run</h1>
      </div>

      {/* B4b — a clone says what it carried and what it could not (the
          tool-approval posture came across, the credentials deliberately did
          not). A prefilled form that looks hand-typed is the failure mode: the
          operator would have no way to know. Review F4: a composer launch's
          RunPrefill carries source:"composer", skipping this banner. */}
      {prefill && prefill.source !== "composer" && (
        <div
          role="status"
          className="mb-6 space-y-1 rounded-lg border border-border bg-muted/40 px-3 py-2.5 text-xs leading-relaxed text-muted-foreground"
        >
          <p>{RUN.CLONE_NOTE}</p>
          {/* Create re-clamps against the caller's governance ceiling, so a
              member cloning an admin's run is narrowed at launch — said here,
              before Launch, rather than as a warning after it. */}
          <p>{RUN.CLONE_CEILING_NOTE}</p>
          {/* An inline policy is never stored, so the barrier below is this
              wizard's default, not the original run's document. */}
          {prefill.inlinePolicy && <p className="text-warning">{RUN.CLONE_INLINE_POLICY_CEILING}</p>}
        </div>
      )}

      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_20rem]">
        {/* Left: the form */}
        <div className="min-w-0 space-y-4">
          {/* Identity first: the one thing that makes this run findable a week
              from now. Title is optional (#1197 L2) — it defaults from the
              task's own first line and stays editable. */}
          <SectionCard title="This run">
            <div className="space-y-4">
              <Field label="Title" htmlFor="nr-title">
                <Input
                  id="nr-title"
                  // Rulebook §8: default focus lands on the primary field.
                  autoFocus
                  // NO Enter-to-launch: this input carries the datalist below,
                  // and Chrome dispatches keydown Enter when a suggestion is
                  // picked, which would LAUNCH the run. Launch is the rail's
                  // button only.
                  maxLength={200}
                  // Native datalist: existing titles are offered as you type,
                  // so joining a family is a pick, not an exact retype. No combobox
                  // library, and typing something new still just works.
                  list="nr-known-titles"
                  placeholder="Refactor the payments module"
                  value={state.title}
                  // #1197 L2: any edit — including clearing it — turns off the
                  // task-derived default for the rest of this session.
                  onChange={(e) => {
                    setTitleUserEdited(true);
                    patch({ title: e.target.value });
                  }}
                />
                <datalist id="nr-known-titles">
                  {knownTitles.map((t) => (
                    <option key={t} value={t} />
                  ))}
                </datalist>
              </Field>

              <Field
                label="Description"
                htmlFor="nr-description"
                hint="Optional. Why this run exists — for whoever reads it later."
              >
                <Textarea
                  id="nr-description"
                  rows={2}
                  maxLength={2000}
                  placeholder="Ticket 4412 — the refund path double-charges on retry."
                  value={state.description}
                  onChange={(e) => patch({ description: e.target.value })}
                />
              </Field>
            </div>
          </SectionCard>

          <WhatToRunStep
            state={state}
            patch={patch}
            isAgent={isAgent}
            isInteractive={isInteractive}
            agentName={agentName}
            harnesses={harnesses}
          />

          {/* The Select, the ungranted-selection reason and the member's own
              drive block — see workspace-card.tsx for why the drive lives
              under the Select rather than in the Add-workspace dialog. */}
          <WorkspaceCard
            state={state}
            patch={patch}
            workspaces={workspaces}
            caps={caps}
            modelProviders={modelProviders}
            onAddWorkspace={() => setAddWsOpen(true)}
            drive={userDrive}
            driveDeniedBy={driveDeniedBy}
            driveUnavailable={driveUnavailable}
          />

          {/* #214 — the Barrier control OUT of the Policy card and into its
              own section: it decides whether a run is confined at all, and
              it was the hardest thing on the screen to find. */}
          <SectionCard title="Barrier">
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
          </SectionCard>

          <SectionCard title="Policy">
            <div className="space-y-4">
              <PolicyPanel
                instance="run"
                value={specText}
                onChange={onSpecChange}
                onPreflight={preflight}
                preflightBusy={preflighting}
                preflightDisabled={(policyMode === "saved" && !state.selectedPolicyId) || !!gates.defaultWorkspaceProblem}
                interactive={isInteractive}
                adoCeiling={adoCeiling}
                modelProviders={modelProviders}
                policyMode={{
                  mode: policyMode,
                  defaultPolicy: {
                    status: defaultRead.status,
                    spec: defaultPolicy && previewSpec(defaultPolicy),
                    profileName: governanceProfile,
                    problem: gates.defaultWorkspaceProblem,
                    onRetry: retryDefault,
                  },
                  onModeChange: onPolicyModeChange,
                  picker: (
                    <div className="space-y-2">
                      <Select value={state.selectedPolicyId ?? ""} onValueChange={onPickPolicy}>
                        <SelectTrigger aria-label="Saved policy">
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
                      {adoAccess?.token_mode === "minted_pat" ? (
                        <>
                          <AdoRunTokenLine
                            policyCaps={savedPolicies.find((p) => p.id === state.selectedPolicyId)?.spec.azure_devops_capabilities}
                            defaults={adoAccess.default_profile}
                          />
                        </>
                      ) : (
                        <ADOAccessSummary caps={savedPolicies.find((p) => p.id === state.selectedPolicyId)?.spec.azure_devops_capabilities} />
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
                              <Link to="/admin/policies" className="font-medium text-info hover:underline">
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

              {/* A launch on a row that creates a token per run is refused until the
                  person has connected (or while the organisation blocks it). */}
              <AdoLaunchNote access={adoAccess} refusal={policy.adoDoor.refusal} connecting={policy.adoDoor.dialog.connecting} onConnect={policy.adoDoor.dialog.onConfirm} />

              {/* C5: named, not left to the barrier above silently winning. */}
              {policyMode === "custom" && policy.unparseableFloor && (
                <p className="text-xs text-warning">{AGENTS.FLOOR_UNPARSEABLE(policy.unparseableFloor)}</p>
              )}

              {/* What buildSpec unions in AFTER the parse, named out loud. A
                  policy the operator did not write is one they cannot be held
                  to — and these are exactly the entries the document itself
                  cannot know: the Workspace card's attachments, the grant
                  lanes' grants, and the hosts those grants must reach (an
                  api_key grant whose host is not on the allowlist
                  authenticates nothing, allow_all_egress included). Saved-policy runs launch by REFERENCE, so nothing
                  is merged into a stored spec. */}
              {policyMode === "custom" && hasAdditions && added && (
                <div
                  className="rounded-lg border border-border bg-surface-2 p-3"
                  data-testid="run-spec-additions"
                >
                  <p className="text-xs text-muted-foreground">
                    Added for this run&apos;s selections:
                  </p>
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
          </SectionCard>
        </div>

        {/* Right: the live rail, a fixed 320px. startup, workspaceUnavailable and
            problem are derived inside the panel from the same raw fields Launch
            reads, so the rail cannot describe or gate one run while Launch
            sends another. */}
        <NewRunLaunchPanel
          governanceProfile={governanceProfile}
          governanceContact={governanceContact}
          savedPolicy={policy.selectedPolicy}
          cc={cc}
          showModelWarning={isAgent && llmReady === false}
          isInteractive={isInteractive}
          interactiveStart={state.interactiveStart}
          mode={state.mode}
          agent={state.agent}
          toolApprovals={state.toolApprovals}
          toolRules={policy.toolRules}
          pushRules={pushRules}
          unattended={unattended}
          onLaunch={launch}
          launchDisabled={launchDisabled}
          launchSpinning={launchSpinning}
          launching={launching}
          task={state.task}
          policyMode={policyMode}
          specParsedOk={policy.parsed.ok}
          selectedPolicyId={state.selectedPolicyId}
          policiesLoaded={policiesLoaded}
          pin={pin}
          workspaces={workspaces}
          selectedWorkspaceId={state.workspaces[0]?.workspaceId}
          attachedWorkspaces={state.workspaces.length}
          caps={caps}
          modelProviders={modelProviders}
          noBarrier={policy.noBarrierOnHost}
          runnerUnknown={!availableClasses}
          error={error}
          errorSeq={errorSeq}
          errorPolicy={errorPolicy}
          credentialRefused={credentialRefused}
          refusedProvider={refusedProvider}
          launchBody={currentBody}
          draftRevision={draftRevision}
          onPreflight={async () => invalidateChecks()}
          preflightRefusal={preflightRefusal}
          preflightIsCurrent={preflightIsCurrent}
          preflightFresh={preflightFresh}
          preflightBlock={preflightBlock}
          preflightChecking={preflighting}
          preflightNotChecked={preflightNotChecked}
          preflightError={preflightError}
          preflightErrorSeq={preflightErrorSeq}
          preflightResult={preflightResult}
          agentRow={isAgent ? harnesses?.find((h) => h.id === state.agent) : undefined}
          isAgent={isAgent}
          providerCandidates={providerCandidates}
          providerAccess={providerAccess}
          selectedModelProviderId={state.modelProviderId}
          onModelProviderChange={onModelProviderChange}
          providerChangeNote={providerChangeNote}
          providerGateState={providerGateState}
          agentName={agentName}
          adoDialog={policy.adoDoor.dialog}
        />
      </div>

      {addWsOpen && (
        <AddWorkspaceDialog
          existingNames={workspaces.map((w: Workspace) => w.name)}
          onClose={() => setAddWsOpen(false)}
          onCreated={() => {
            setAddWsOpen(false);
            reloadWorkspaces();
            toast.success("Workspace added");
          }}
        />
      )}
    </div>
  );
}
