/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// PermissionWizard — the New Run flow. A multi-step Dialog that composes the
// CANONICAL wire contract and, on launch, optionally persists the spec as a named
// policy (createPolicy) then creates the run (createRun) with inline_policy.
import * as React from "react";
import { ArrowLeft, ArrowRight, KeyRound, Loader2, Rocket } from "lucide-react";
import type {
  AgentRun,
  ConfinementClass,
  PreflightResult,
  Workspace,
  WorkspaceProfile,
} from "../../../lib/types";
import { policies as policiesApi } from "../../../lib/api/policies";
import { runs as runsApi } from "../../../lib/api/runs";
import { health as healthApi } from "../../../lib/api/health";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { getErrorMessage as msg } from "../../../lib/format";
import { getDefaultCc, resolveDefaultCc } from "../../wardyn/default-confinement";
import { parseMissingSecret, surfaceRunWarnings, useAddSecretFix } from "./run-warnings";
import { Button } from "../../ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "../../ui/dialog";
import { StepIndicator } from "./step-shell";
import { StepBasics } from "./step-basics";
import { StepAccess } from "./step-access";
import { StepEgress } from "./step-egress";
import { StepConfinement } from "./step-confinement";
import { StepReview } from "./step-review";
import { AddSecretDialog } from "../secrets";
import { AddWorkspaceDialog } from "../workspaces";
import {
  WIZARD_STEPS,
  applyProfileSpecToState,
  buildSpec,
  initialWizardState,
  validateStep,
  type WizardState,
  type WizardStepId,
} from "./wizard-types";

export function PermissionWizard({
  open,
  onOpenChange,
  onCreated,
  initialState,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onCreated: (run: AgentRun) => void;
  // When provided (e.g. "Edit in wizard" from a composer proposal) the wizard
  // opens prefilled with this state instead of a clean default. The confinement-
  // class floor probe still runs, but it does NOT overwrite a prefilled state.
  initialState?: WizardState;
}) {
  const [stepIdx, setStepIdx] = React.useState(0);
  const [state, setState] = React.useState<WizardState>(
    () => initialState ?? initialWizardState("CC1"),
  );
  // null = the health probe hasn't resolved yet — StepConfinement renders
  // "Checking…" instead of a definitive (and possibly false) hardware reason.
  const [availableClasses, setAvailableClasses] = React.useState<string[] | null>(null);
  const [secrets, setSecrets] = React.useState<string[]>([]);
  const [secretsLoading, setSecretsLoading] = React.useState(false);
  const [workspaces, setWorkspaces] = React.useState<Workspace[]>([]);
  const [workspacesLoading, setWorkspacesLoading] = React.useState(false);
  // Workspaces whose recorded egress has already been merged into the Egress step,
  // so re-merging never fights the operator trimming a host. Reset per dialog-open.
  const mergedWs = React.useRef<Set<string>>(new Set());
  const [addWorkspaceOpen, setAddWorkspaceOpen] = React.useState(false);
  const [launching, setLaunching] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  // Review preflight (advisory, non-blocking): a DRY-RUN of launch's resolution +
  // gating so Review can show the setup checklist + the confinement class the run
  // will ACTUALLY run at, before the operator commits. null until it resolves.
  const [preflight, setPreflight] = React.useState<PreflightResult | null>(null);
  const [preflightStatus, setPreflightStatus] = React.useState<"idle" | "loading" | "error">(
    "idle",
  );
  // A launch failure that names a not-yet-stored secret (H1: the stored/default
  // policy path now 422s on this too, same as inline) — offers the same
  // one-click fix the composer review panel does.
  const missingSecret = React.useMemo(() => parseMissingSecret(error), [error]);

  const step = WIZARD_STEPS[stepIdx];
  const patch = React.useCallback(
    (p: Partial<WizardState>) => setState((s) => ({ ...s, ...p })),
    [],
  );

  const loadSecrets = React.useCallback(() => {
    setSecretsLoading(true);
    secretsApi
      .listSecrets()
      .then(setSecrets)
      .catch(() => setSecrets([]))
      .finally(() => setSecretsLoading(false));
  }, []);

  const loadWorkspaces = React.useCallback(() => {
    setWorkspacesLoading(true);
    workspacesApi
      .listWorkspaces()
      .then(setWorkspaces)
      .catch(() => setWorkspaces([]))
      .finally(() => setWorkspacesLoading(false));
  }, []);

  // Recorded-profile fast-track: selecting a profile (a workspace recording) loads
  // its synthesized least-privilege spec (runsApi.profileRun) into steps 2-4. Profiles
  // are tied to the workspace by BEING its recordings — no name/policy matching.
  const [profileLoading, setProfileLoading] = React.useState(false);
  const applyProfile = async (runId: string, key: string) => {
    setProfileLoading(true);
    setError(null);
    try {
      const p = await runsApi.profileRun(runId);
      patch(applyProfileSpecToState(state, p.proposed.inline_policy, workspaces, key));
    } catch (e) {
      setError("Couldn't load that recorded profile: " + msg(e));
    } finally {
      setProfileLoading(false);
    }
  };

  // On open: probe runner capabilities (confinement gating) + load secret names.
  // With no prefilled initialState, reset to a clean state whose default barrier is
  // the operator's persisted Getting-started pick if this host still runs it, else
  // the strongest available tier (resolveDefaultCc) — prefer-strongest/persisted,
  // aligning with the Getting-started selection ring, not a prefer-weakest floor. With a
  // prefilled initialState (e.g. "Edit in wizard" from a composer proposal) seed
  // that state and DON'T overwrite it from the probe — the proposal already carries
  // its confinement class.
  React.useEffect(() => {
    if (!open) return;
    setStepIdx(0);
    setError(null);
    setLaunching(false);
    setPreflight(null);
    setPreflightStatus("idle");
    mergedWs.current = new Set();
    if (initialState) setState(initialState);
    setAvailableClasses(null);
    let alive = true;
    // M19 fix: healthApi.health() never rejects — it swallows a fetch/parse failure
    // into {} (see api.ts) — so the old .catch below was dead code, and an
    // empty/failed probe fell straight into .then() instead. That was then
    // read as a DEFINITIVE "this runner has zero barriers", rendering "No
    // Wall/Vault runtime on this runner" even on a transient blip (the server
    // side, handleHealthz, ALSO emits an empty confinement_classes list on its
    // own Runner error path, not only for a genuinely CC1-only host). Empty
    // means unknown, not confirmed-absent — retry once before ever committing
    // to it; only a still-empty result after the retry settles into the
    // documented CC1-only fallback below.
    let retried = false;
    const probe = () => {
      healthApi.health().then((h) => {
        if (!alive) return;
        const classes = (h.confinement_classes ?? []).filter(Boolean);
        if (classes.length === 0 && !retried) {
          retried = true;
          probe();
          return;
        }
        setAvailableClasses(classes);
        if (initialState) return; // keep the prefilled proposal verbatim
        setState(initialWizardState(resolveDefaultCc(getDefaultCc(), classes as ConfinementClass[])));
      });
    };
    probe();
    loadSecrets();
    loadWorkspaces();
    return () => {
      alive = false;
    };
  }, [open, loadSecrets, loadWorkspaces, initialState]);

  // When a workspace is selected, load its recorded profile's egress (approved_egress
  // ∪ scanned registries) into the Egress step so a new run VISIBLY inherits the
  // recording — the operator sees the hosts and can trim them. Merge-once per
  // workspace (mergedWs) so trimming doesn't snap back. The server unions the same
  // set at launch (referencedWorkspaces → unionWorkspaceEgress); this surfaces it.
  React.useEffect(() => {
    if (workspaces.length === 0) return;
    const add = new Set<string>();
    for (const sel of state.workspaces) {
      if (mergedWs.current.has(sel.workspaceId)) continue;
      const w = workspaces.find((x) => x.id === sel.workspaceId);
      if (!w) continue;
      mergedWs.current.add(sel.workspaceId);
      for (const h of w.approved_egress ?? []) add.add(h);
      const prof = (w.profile ?? {}) as WorkspaceProfile;
      for (const h of prof.egress_domains ?? []) add.add(h);
    }
    if (add.size === 0) return;
    patch({ allowedDomains: Array.from(new Set([...state.allowedDomains, ...add])) });
  }, [state.workspaces, workspaces, state.allowedDomains, patch]);

  const stepError = validateStep(step.id, state);
  const isLast = stepIdx === WIZARD_STEPS.length - 1;

  const goNext = () => {
    if (stepError) {
      setError(stepError);
      return;
    }
    setError(null);
    setStepIdx((i) => Math.min(i + 1, WIZARD_STEPS.length - 1));
  };
  const goBack = () => {
    setError(null);
    setStepIdx((i) => Math.max(i - 1, 0));
  };
  // Fast-track a selected saved profile straight to Review (skips Access/Egress/
  // Confinement, which the profile already populated). The Review step + launch still
  // validate the full spec, so nothing unsafe is skipped — only the manual clicks.
  const goToReview = () => {
    if (stepError) {
      setError(stepError);
      return;
    }
    setError(null);
    setStepIdx(WIZARD_STEPS.findIndex((s) => s.id === "review"));
  };
  const jumpTo = (id: WizardStepId) => {
    const idx = WIZARD_STEPS.findIndex((s) => s.id === id);
    if (idx <= stepIdx) {
      setError(null);
      setStepIdx(idx);
    }
  };

  // A DRY-RUN of launch with the SAME body createRun would send: it resolves the
  // policy through the real chokepoint (real 4xx errors), returns the setup
  // checklist + the enforced confinement class, and mints/persists nothing. Any
  // failure is swallowed to a quiet "preflight unavailable" — it NEVER blocks Review.
  const runPreflight = React.useCallback(async () => {
    setPreflightStatus("loading");
    try {
      const { run, inline_policy } = buildSpec(state, workspaces);
      setPreflight(await runsApi.preflightRun({ ...run, inline_policy }));
      setPreflightStatus("idle");
    } catch {
      setPreflight(null);
      setPreflightStatus("error");
    }
  }, [state, workspaces]);
  // A ref so the enter-Review effect fires ONCE per entry with the latest spec —
  // never re-firing on unrelated Review-step edits (e.g. typing a profile name).
  const runPreflightRef = React.useRef(runPreflight);
  runPreflightRef.current = runPreflight;
  React.useEffect(() => {
    if (step.id === "review") void runPreflightRef.current();
  }, [step.id]);

  const launch = async () => {
    // Re-validate every step before launch so a skipped requirement can't slip in.
    for (const s of WIZARD_STEPS) {
      const err = validateStep(s.id, state);
      if (err) {
        setError(err);
        setStepIdx(WIZARD_STEPS.findIndex((w) => w.id === s.id));
        return;
      }
    }
    setError(null);
    setLaunching(true);
    try {
      const { run, inline_policy } = buildSpec(state, workspaces);
      const created = await runsApi.createRun({ ...run, inline_policy });
      if (state.saveAsProfile && state.profileName.trim()) {
        // M14 fix: persist the named policy AFTER the run launches, not before.
        // createPolicy used to run first, so a failed launch (e.g. the
        // one-click missing-secret fix retrying) had already created the named
        // policy — the retry's createPolicy call then hit the policies-name
        // UNIQUE constraint and dead-ended. Best-effort companion to the run
        // (the run itself carries inline_policy, so it's self-contained): a
        // save-as-profile failure here (e.g. reusing an existing name) must
        // not undo a run that already launched successfully.
        try {
          await policiesApi.createPolicy(state.profileName.trim(), inline_policy);
        } catch {
          /* best-effort — the run already launched */
        }
      }
      setLaunching(false);
      onOpenChange(false);
      onCreated(created);
      // Advisory POST /runs warnings (e.g. a workspace-dir collision): the run
      // already launched, so surface them without blocking.
      surfaceRunWarnings(created);
    } catch (e) {
      setError(msg(e) || "Failed to launch run.");
      setLaunching(false);
    }
  };

  // Shared add-secret recovery (run-warnings.ts): a manual "Add secret" applies the
  // saved name to llmSecretName; the launch-error fix re-launches once the named
  // secret exists (H3, mirrors the composer review panel). Both reload the list.
  const secretFix = useAddSecretFix({
    onManual: (name) => {
      loadSecrets();
      patch({ llmSecretName: name });
      // Refresh the Review checklist so a just-stored secret flips to Configured.
      void runPreflightRef.current();
    },
    onRetry: () => {
      loadSecrets();
      void launch();
    },
  });

  return (
    <>
      <Dialog open={open} onOpenChange={onOpenChange}>
        <DialogContent className="flex max-h-[88vh] flex-col gap-0 sm:max-w-2xl lg:max-w-5xl xl:max-w-6xl">
          <DialogHeader className="border-b border-border pb-4">
            <DialogTitle>New run</DialogTitle>
            <DialogDescription>
              Compose the agent's permission envelope. The run is launched under an inline policy —
              no admin pre-provisioning required.
            </DialogDescription>
            <div className="pt-3">
              <StepIndicator current={step.id} onJump={jumpTo} />
            </div>
          </DialogHeader>

          <div className="scroll-thin -mx-1 flex-1 overflow-y-auto px-1 py-4">
            {step.id === "basics" && (
              <StepBasics
                state={state}
                patch={patch}
                workspaces={workspaces}
                workspacesLoading={workspacesLoading}
                profileLoading={profileLoading}
                onSelectProfile={applyProfile}
                onClearProfile={() => patch({ selectedProfile: undefined })}
                onAddWorkspace={() => setAddWorkspaceOpen(true)}
              />
            )}
            {step.id === "access" && (
              <StepAccess
                state={state}
                patch={patch}
                secrets={secrets}
                secretsLoading={secretsLoading}
                onAddSecret={() => secretFix.openManual()}
              />
            )}
            {step.id === "egress" && <StepEgress state={state} patch={patch} />}
            {step.id === "confinement" && (
              <StepConfinement
                state={state}
                patch={patch}
                availableClasses={availableClasses}
                minClass={initialState?.confinementClass}
              />
            )}
            {step.id === "review" && (
              <StepReview
                state={state}
                patch={patch}
                workspaces={workspaces}
                preflight={preflight}
                preflightStatus={preflightStatus}
                onAddSecret={(name) => secretFix.openManual(name)}
                onFixWorkspace={(id) => {
                  // Same scan-and-refresh pattern AddWorkspaceDialog uses, then
                  // re-run preflight so the checklist reflects the new scan status.
                  workspacesApi
                    .scanWorkspace(id)
                    .catch(() => {})
                    .finally(() => {
                      loadWorkspaces();
                      void runPreflightRef.current();
                    });
                }}
              />
            )}
          </div>

          {error && (
            <div
              className="mb-3 space-y-2 rounded-lg border border-danger/30 bg-danger-subtle px-3 py-2 text-xs text-danger"
              data-testid="wizard-launch-error"
            >
              <p>{error}</p>
              {missingSecret && (
                <Button
                  size="sm"
                  variant="outline"
                  className="gap-1.5"
                  onClick={() => secretFix.openFix(missingSecret)}
                >
                  <KeyRound className="size-3.5" /> Add the “{missingSecret}” secret
                </Button>
              )}
            </div>
          )}

          <div className="flex items-center justify-between border-t border-border pt-4">
            <Button variant="ghost" onClick={() => onOpenChange(false)} disabled={launching}>
              Cancel
            </Button>
            <div className="flex items-center gap-2">
              <Button variant="outline" onClick={goBack} disabled={stepIdx === 0 || launching}>
                <ArrowLeft className="size-4" /> Back
              </Button>
              {isLast ? (
                <Button onClick={launch} disabled={launching}>
                  {launching ? (
                    <Loader2 className="size-4 animate-spin" />
                  ) : (
                    <Rocket className="size-4" />
                  )}
                  Launch run
                </Button>
              ) : step.id === "basics" && state.selectedProfile ? (
                // Fast-track: a saved profile populated steps 2-4 — skip straight to Review.
                <Button onClick={goToReview} disabled={!!stepError}>
                  Review now <ArrowRight className="size-4" />
                </Button>
              ) : (
                <Button onClick={goNext} disabled={!!stepError}>
                  Next <ArrowRight className="size-4" />
                </Button>
              )}
            </div>
          </div>
        </DialogContent>
      </Dialog>

      <AddSecretDialog {...secretFix.dialogProps} existingNames={secrets} />

      <AddWorkspaceDialog
        open={addWorkspaceOpen}
        onOpenChange={setAddWorkspaceOpen}
        onSaved={(ws) => {
          // Auto-attach the newly onboarded workspace so the operator doesn't
          // have to re-open the picker to select what they just added.
          patch({ workspaces: [...state.workspaces, { workspaceId: ws.id }] });
          loadWorkspaces();
          // Best-effort scan (matches the Workspaces screen): a local dir reaches
          // "ready" inline, a repo launches its governed scan run — so the inline
          // path isn't left stuck in pending_scan. Refresh once it settles.
          workspacesApi.scanWorkspace(ws.id).catch(() => {}).finally(loadWorkspaces);
        }}
      />
    </>
  );
}
