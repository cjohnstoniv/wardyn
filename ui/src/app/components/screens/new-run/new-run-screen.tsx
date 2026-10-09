/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New run — four panels you may take in any order (Run, Workspace, Access,
// Policy) beside a live rail that answers "what can this run actually do?"
// while you build it, with Launch reachable from every one of them (#1922).
//
// A RE-LAYOUT, not a re-model: buildSpec()/impliedEgressHosts() are reused
// verbatim, so the launch payload is exactly what the single page produced —
// the governance contract is the tested part; this is about when the operator
// SEES it, not what it is. Every panel stays mounted: moving between them
// changes what is on screen, never the draft.
import * as React from "react";
import { ArrowLeft } from "lucide-react";
import { toast } from "sonner";
import type { Workspace } from "../../../lib/types";
import { Button } from "../../ui/button";
import { RUN } from "../../wardyn/copy";
import { AddWorkspaceDialog } from "../add-workspace-dialog";
import { AccessPanel } from "./access-panel";
import { issueCounts, shownIssue, ISSUE_TARGET } from "./new-run-launch-gates";
import { NewRunLaunchPanel } from "./new-run-launch-panel";
import { Panel, PanelNav, useNewRunPanels } from "./panel-nav";
import { PolicyPanelBody } from "./policy-panel-body";
import { RunPanel } from "./run-panel";
import { useFocusIssue } from "./use-focus-issue";
import { useNewRunController } from "./use-new-run-controller";
import { runPromptText } from "./wizard-types";
import { WorkspaceCard } from "./workspace-card";

export function NewRunScreen() {
  const c = useNewRunController();
  const {
    leave, guardLink, issues, prefill, state, setTitleUserEdited,
    patch, knownTitles, isAgent, isInteractive,
    agentName, harnesses, workspaces, caps,
    modelProviders, setAddWsOpen, userDrive, driveDeniedBy,
    driveUnavailable, policy, cc,
    availableClasses, policyMode,
    governanceProfile, savedPolicies,
    adoAccess,
    governanceContact, llmReady, pushRules, unattended,
    launch, launchDisabled, launchSpinning, launching,
    policiesLoaded, pin, error, errorSeq,
    errorPolicy, credentialRefused, refusedProvider, currentBody, draftRevision,
    preflightRefusal, invalidateChecks, preflighting, preflightIsCurrent, preflightFresh, preflightBlock,
    preflightNotChecked, preflightError, preflightErrorSeq, preflightResult,
    providerCandidates, providerAccess, onModelProviderChange, providerChangeNote,
    providerGateState, addWsOpen, reloadWorkspaces,
    setPolicyEditing, setPolicyView,
    accessRows, accessIssues, operator,
  } = c;
  const { panel, go, reveal } = useNewRunPanels();
  // One Access row is open at a time; an issue's link opens its row first.
  const [openRowId, setOpenRowId] = React.useState<string | undefined>(undefined);
  const focusIssue = useFocusIssue(reveal, setOpenRowId);
  // The issue the line above Launch names; a control it belongs to points its
  // description there.
  const shown = launching ? null : shownIssue(issues, panel);
  const modelProvider = isAgent
    ? {
        candidates: providerCandidates,
        access: providerAccess,
        selectedId: state.modelProviderId,
        onChange: onModelProviderChange,
        changeNote: providerChangeNote,
        gate: providerGateState,
        harnessLabel: agentName,
      }
    : undefined;

  return (
    <div className="mx-auto w-full max-w-[1200px] px-6 py-6">
      <div className="mb-4 flex items-center gap-3">
        <Button variant="ghost" size="sm" onClick={leave}>
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
          className="mb-4 space-y-1 rounded-lg border border-border bg-muted/40 px-3 py-2.5 text-xs leading-relaxed text-muted-foreground"
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

      <div className="mb-4">
        <PanelNav active={panel} counts={issueCounts(issues)} onSelect={go} />
      </div>

      {/* A flex column below lg, not a one-column grid: a grid item sticks
          only inside its own row, and the rail has to stick to the bottom of
          the whole page as its footer. */}
      <div className="flex flex-col gap-4 lg:grid lg:grid-cols-[minmax(0,1fr)_20rem]">
        <div className="min-w-0">
          <Panel id="run" active={panel} onSelect={go}>
            <RunPanel
              state={state}
              patch={patch}
              isAgent={isAgent}
              isInteractive={isInteractive}
              agentName={agentName}
              harnesses={harnesses}
              setTitleUserEdited={setTitleUserEdited}
              knownTitles={knownTitles}
              modelProvider={modelProvider}
              issues={issues}
              shown={shown}
              toolRules={policy.toolRules}
              onToolRules={() => {
                // A saved or default policy is read-only here: the link lands
                // on its Summary. A custom one opens for editing, on the rules
                // when the source parses and on the source when it does not.
                if (policyMode !== "custom") {
                  setPolicyView("summary");
                  go("policy", ISSUE_TARGET.POLICY_READ);
                  return;
                }
                setPolicyEditing(true);
                go("policy", policy.parsed.ok ? ISSUE_TARGET.TOOL_RULES : ISSUE_TARGET.POLICY_SOURCE);
              }}
            />
          </Panel>

          <Panel id="workspace" active={panel} onSelect={go}>
            {/* The Select, the reason a selection holds Launch and the member's
                own drive block — see workspace-card.tsx for why the drive lives
                under the Select rather than in the Add-workspace dialog. */}
            <WorkspaceCard
              active={panel === "workspace"}
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
          </Panel>

          <Panel id="access" active={panel} onSelect={go}>
            <AccessPanel
              adoAccess={adoAccess}
              savedMode={policyMode === "saved"}
              savedCaps={savedPolicies.find((p) => p.id === state.selectedPolicyId)?.spec.azure_devops_capabilities}
              adoRefusal={policy.adoDoor.refusal}
              adoConnecting={policy.adoDoor.dialog.connecting}
              onAdoConnect={policy.adoDoor.dialog.onConfirm}
              rows={accessRows}
              openRowId={openRowId}
              onOpenRow={setOpenRowId}
              secretsPath={operator ? "/admin/secrets" : "/secrets"}
              guardLink={guardLink}
            />
          </Panel>

          <Panel id="policy" active={panel} onSelect={go}>
            <PolicyPanelBody c={c} active={panel === "policy"} issues={issues} shown={shown} guardLink={guardLink} />
          </Panel>
        </div>

        {/* The live rail. startup and the issue it names are derived inside the
            panel from the same raw fields Launch reads, so the rail cannot
            describe or gate one run while Launch sends another. */}
        <NewRunLaunchPanel
          panel={panel}
          onIssue={(issue) => {
            // The source is on screen only while editing: its issue opens the editor first.
            if (issue.focus === ISSUE_TARGET.POLICY_SOURCE) setPolicyEditing(true);
            focusIssue(issue);
          }}
          guardLink={guardLink}
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
          task={runPromptText(state)}
          title={state.title}
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
          accessIssues={accessIssues}
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
