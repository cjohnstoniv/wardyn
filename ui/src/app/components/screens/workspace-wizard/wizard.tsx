/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "Add workspace" wizard — the single front door replacing the old
// 6-step import dialog. Rail: Sources -> Base image -> Requirements -> Done.
// Owns the rail + all step state; each step-*.tsx is a pure controlled view.
// Mirrors mockup/wardyn-workspaces.js's AddWorkspaceWizard/V2 shell (Dialog +
// StepRail + scrollable body + footer), rebuilt against the REAL API surface
// instead of the mock's in-memory fixture store.
import * as React from "react";
import { toast } from "sonner";
import { Button } from "../../ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../../ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../../ui/alert-dialog";
import { StepIndicator } from "../new-run/step-shell";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { setup as setupApi } from "../../../lib/api/setup";
import { integrationsApi } from "../../../lib/api/integrations";
import { slugHost } from "../../../lib/scm-provider";
import { getErrorMessage } from "../../../lib/format";
import { C, V2C } from "../../../lib/workspace-copy";
import type { WorkspaceProfile } from "../../../lib/types";
import { StepSources } from "./step-sources";
import { StepBaseImage } from "./step-base-image";
import { StepRequirements } from "./step-requirements";
import { StepDone, type DoneVariant } from "./step-done";
import {
  WIZARD_STEPS,
  canDriveClaudeCode,
  defaultBaseImageState,
  deriveInitialRequirements,
  isSshRemote,
  newSourceRow,
  parseRepoSource,
  removeSource,
  seedFloor,
  toSourceInput,
  withComposition,
  type BaseImageState,
  type PowerSource,
  type SourceRow,
  type SourceScanState,
  type WizardOrigin,
  type WizardStepId,
  type WorkspaceRequirementsMap,
  type WorkspaceSourceKind,
  type WorkspaceWithComposition,
} from "./wizard-types";

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

interface WizardState {
  step: WizardStepId;
  name: string;
  sources: SourceRow[];
  secretNames: string[];
  githubApp: boolean;
  harnessAvailable: boolean;
  workspace: WorkspaceWithComposition | null;
  scans: Record<string, SourceScanState>;
  scanning: boolean;
  partial: boolean;
  baseImage: BaseImageState;
  powerSource: PowerSource;
  requirements: WorkspaceRequirementsMap;
  requirementsSeeded: boolean;
  confirmBackToSources: boolean;
  creating: boolean;
  savingRequirements: boolean;
}

function initialState(): WizardState {
  return {
    step: "sources",
    name: "",
    sources: seedFloor(),
    secretNames: [],
    githubApp: false,
    harnessAvailable: false,
    workspace: null,
    scans: {},
    scanning: false,
    partial: false,
    baseImage: defaultBaseImageState(),
    powerSource: { kind: "default" },
    requirements: {},
    requirementsSeeded: false,
    confirmBackToSources: false,
    creating: false,
    savingRequirements: false,
  };
}

function profileOf(ws: WorkspaceWithComposition | null): WorkspaceProfile | null {
  return ws ? ((ws.profile ?? null) as WorkspaceProfile | null) : null;
}

function detectedChipsFor(profile: WorkspaceProfile | null): string[] {
  if (!profile) return [];
  return [...(profile.languages ?? []), ...(profile.package_managers ?? [])];
}

// A "scan has happened" gate for the back-to-Sources confirm: once the
// workspace exists past pending_scan, or the operator has already edited the
// requirements contract, going back and re-continuing would discard both
// (C.RESCAN_DESTROYS is written for exactly this: re-reading the source
// clears requirements and recorded sessions reviewed against the old content).
function hasScanned(state: WizardState): boolean {
  if (Object.keys(state.requirements).length > 0) return true;
  const status = state.workspace?.status;
  return !!status && status !== "pending_scan";
}

export function WorkspaceWizard({
  origin = "library",
  onClose,
  onWorkspaceCreated,
  onOpenWorkspace,
  onAttach,
}: {
  origin?: WizardOrigin;
  onClose: () => void;
  /** Fired once the workspace is created, so a host list can refresh. */
  onWorkspaceCreated?: (workspace: WorkspaceWithComposition) => void;
  /** Done's "Make it stronger" cards deep-link here — wiring the real route is
   *  a later step (this wizard isn't mounted into the app yet); omitted, the
   *  cards are inert. */
  onOpenWorkspace?: (workspaceId: string, focus?: "record" | "env" | "model") => void;
  /** origin="run" only — omitted, Done's primary action falls back to closing. */
  onAttach?: (workspaceId: string) => void;
}) {
  const [state, setState] = React.useState<WizardState>(initialState);
  const patch = (p: Partial<WizardState>) => setState((s) => ({ ...s, ...p }));
  const s = state;

  React.useEffect(() => {
    let live = true;
    secretsApi.listSecrets().then((names) => live && patch({ secretNames: names })).catch(() => {});
    setupApi.getSetupStatus().then((status) => live && patch({ githubApp: status.secrets.github_app })).catch(() => {});
    integrationsApi
      .list()
      .then((data) => live && patch({ harnessAvailable: data.ai.some((r) => canDriveClaudeCode(r.aiType)) }))
      .catch(() => {});
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const close = () => onClose();

  // ---------- Sources (step ①) ----------
  const addSource = (type: WorkspaceSourceKind) => patch({ sources: [...s.sources, newSourceRow(type)] });
  const updateSource = (id: string, p: Partial<SourceRow>) =>
    patch({ sources: s.sources.map((r) => (r.id === id ? { ...r, ...p } : r)) });
  const removeSourceRow = (id: string) => patch({ sources: removeSource(s.sources, id) });
  const onSecretStored = (name: string) => patch({ secretNames: [...s.secretNames, name] });

  // ---------- Scan orchestration (Sources -> Base image) ----------
  // ponytail: POST /workspaces/{id}/scan scans the whole composed workspace in
  // one call — there is no per-source scan endpoint yet. Per-source rows still
  // render independently (an SSH-gated row fails immediately, client-side,
  // without waiting on the network), but every OTHER non-ephemeral row shares
  // the one real outcome. Good enough for the wizard's UI; a true per-source
  // signal is a backend addition for later, not a UI-only gap to paper over
  // with invented per-source state.
  const startScan = async (ws: WorkspaceWithComposition) => {
    const nonEphemeral = s.sources.filter((r) => r.type !== "ephemeral");
    const gated = new Set(
      nonEphemeral
        .filter((r) => {
          if (!isSshRemote(r)) return false;
          const host = parseRepoSource(r.source)?.host ?? "";
          return !s.secretNames.includes(`ssh-key-${slugHost(host)}`);
        })
        .map((r) => r.id),
    );
    const now = Date.now();
    const scans: Record<string, SourceScanState> = {};
    for (const r of nonEphemeral) {
      scans[r.id] = gated.has(r.id) ? { status: "failed", error: C.SSH_GATE } : { status: "scanning", startedAt: now };
    }
    patch({ scans, scanning: true, step: "image" });

    if (gated.size === nonEphemeral.length) {
      // Nothing left to actually scan — every real source is SSH-gated.
      patch({ scanning: false });
      return;
    }
    try {
      const { async } = await workspacesApi.scanWorkspace(ws.id);
      let finalWs = ws;
      if (!async) {
        finalWs = withComposition((await workspacesApi.getWorkspace(ws.id)) ?? ws);
      } else {
        for (let i = 0; i < 40; i++) {
          await sleep(1500);
          const polled = await workspacesApi.getWorkspace(ws.id);
          if (polled && polled.status !== "scanning" && polled.status !== "pending_scan") {
            finalWs = withComposition(polled);
            break;
          }
        }
      }
      const failed = finalWs.status === "error";
      const reason = (finalWs as unknown as { scan_error?: string; error?: string }).scan_error
        ?? (finalWs as unknown as { scan_error?: string; error?: string }).error
        ?? "Scan failed — see Runs for details.";
      const next = { ...scans };
      for (const r of nonEphemeral) {
        if (gated.has(r.id)) continue;
        next[r.id] = failed ? { status: "failed", error: reason } : { status: "done" };
      }
      patch({ workspace: finalWs, scans: next, scanning: false });
    } catch (e) {
      toast.error("Scan failed to start", { description: getErrorMessage(e) });
      const next = { ...scans };
      for (const r of nonEphemeral) if (!gated.has(r.id)) next[r.id] = { status: "failed", error: getErrorMessage(e) };
      patch({ scans: next, scanning: false });
    }
  };

  const continueFromSources = async () => {
    if (!s.name.trim()) return;
    if (s.workspace) {
      // Already created (e.g. the operator went back and forward again) —
      // re-source edits beyond the first pass aren't re-sent (updateWorkspace
      // doesn't accept `sources` yet); just re-scan what exists server-side.
      void startScan(s.workspace);
      return;
    }
    patch({ creating: true });
    try {
      const ws = await workspacesApi.createWorkspace({
        name: s.name.trim(),
        sources: s.sources.map((r) => toSourceInput(r, s.sources)),
      });
      const created = withComposition(ws);
      patch({ creating: false, workspace: created });
      onWorkspaceCreated?.(created);
      void startScan(created);
    } catch (e) {
      patch({ creating: false });
      toast.error("Failed to create workspace", { description: getErrorMessage(e) });
    }
  };

  // ---------- Base image (step ②) ----------
  const scannable = s.sources.filter((r) => r.type !== "ephemeral");
  const anyScanning = scannable.some((r) => s.scans[r.id]?.status === "scanning");
  const anyFailed = scannable.some((r) => s.scans[r.id]?.status === "failed");
  const phaseA = s.step === "image" && (anyScanning || anyFailed) && !s.partial;
  const profile = profileOf(s.workspace);
  const detectedChips = detectedChipsFor(profile);

  // ---------- Back-to-Sources gate (C.RESCAN_DESTROYS-style warning) ----------
  const goToSources = () => {
    if (hasScanned(s)) {
      patch({ confirmBackToSources: true });
    } else {
      patch({ step: "sources" });
    }
  };
  const confirmGoToSources = () => {
    patch({
      step: "sources",
      confirmBackToSources: false,
      requirements: {},
      requirementsSeeded: false,
      scans: {},
      partial: false,
    });
  };

  // ---------- Requirements (step ③) ----------
  React.useEffect(() => {
    if (s.step !== "reqs" || s.requirementsSeeded) return;
    const localDirPaths = s.sources.filter((r) => r.type === "local_dir").map((r) => r.path).filter(Boolean);
    patch({
      requirements: deriveInitialRequirements(profile, localDirPaths, s.requirements),
      requirementsSeeded: true,
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [s.step, s.requirementsSeeded]);

  const acceptAndFinish = async () => {
    if (!s.workspace) return;
    patch({ savingRequirements: true });
    try {
      const updated = await workspacesApi.setRequirements(s.workspace.id, s.requirements);
      patch({ savingRequirements: false, workspace: withComposition(updated), step: "done" });
    } catch (e) {
      patch({ savingRequirements: false });
      toast.error("Failed to save requirements", { description: getErrorMessage(e) });
    }
  };

  // ---------- Done (step ④) ----------
  const doneVariant: DoneVariant = !s.workspace
    ? "scanning"
    : s.workspace.status === "error"
      ? "failed"
      : s.workspace.status === "scanning" || s.workspace.status === "pending_scan"
        ? "scanning"
        : "usable";
  const leakCount = profile?.leak_findings?.length ?? 0;

  const wsExists = !!s.workspace;
  const blurb =
    s.step === "sources"
      ? V2C.S1_BLURB
      : s.step === "image"
        ? V2C.S2_BLURB
        : s.step === "reqs"
          ? C.S3_BLURB
          : undefined;

  return (
    <Dialog open onOpenChange={(o) => !o && close()}>
      <DialogContent className="flex max-h-[85vh] flex-col gap-0 p-0 sm:max-w-2xl">
        <div className="space-y-2.5 border-b border-border px-6 py-4">
          <DialogHeader>
            <DialogTitle>Add workspace</DialogTitle>
            {blurb && <DialogDescription>{blurb}</DialogDescription>}
          </DialogHeader>
          <StepIndicator
            steps={WIZARD_STEPS}
            current={s.step}
            onJump={(id) => {
              if (id === "sources") goToSources();
              else if (id === "image" && wsExists) patch({ step: "image" });
              else if (id === "reqs" && wsExists && profile) patch({ step: "reqs" });
            }}
          />
        </div>

        <div className="scroll-thin flex-1 overflow-y-auto px-6 py-5">
          {s.step === "sources" && (
            <StepSources
              name={s.name}
              onNameChange={(name) => patch({ name })}
              sources={s.sources}
              onAddSource={addSource}
              onUpdateSource={updateSource}
              onRemoveSource={removeSourceRow}
              secretNames={s.secretNames}
              githubApp={s.githubApp}
              onSecretStored={onSecretStored}
            />
          )}
          {s.step === "image" && (
            <StepBaseImage
              sources={s.sources}
              scans={s.scans}
              phaseA={phaseA}
              partial={s.partial}
              onEditSource={() => goToSources()}
              onRescan={() => s.workspace && void startScan(s.workspace)}
              detectedChips={detectedChips}
              harnessAvailable={s.harnessAvailable}
              state={s.baseImage}
              onChange={(p) => patch({ baseImage: { ...s.baseImage, ...p } })}
              powerSource={s.powerSource}
              onPowerSourceChange={(powerSource) => patch({ powerSource })}
            />
          )}
          {s.step === "reqs" && (
            <StepRequirements
              profile={profile}
              sources={s.sources}
              requirements={s.requirements}
              onChange={(requirements) => patch({ requirements })}
              storedSecretNames={s.secretNames}
              onSecretStored={onSecretStored}
            />
          )}
          {s.step === "done" && (
            <StepDone
              name={s.name || s.workspace?.name || "workspace"}
              variant={doneVariant}
              requirements={s.requirements}
              storedSecretNames={s.secretNames}
              leakCount={leakCount}
              powerSource={s.powerSource}
              onOpenDetail={(focus) => {
                if (s.workspace) onOpenWorkspace?.(s.workspace.id, focus);
              }}
              onRescan={() => {
                if (s.workspace) onOpenWorkspace?.(s.workspace.id);
              }}
            />
          )}
        </div>

        <DialogFooter className="flex-col gap-2 border-t border-border px-6 py-4 sm:flex-col">
          <div className="flex w-full flex-wrap justify-end gap-2">
            {s.step === "sources" && (
              <>
                <Button type="button" variant="outline" onClick={close}>
                  {wsExists ? "Close" : "Cancel"}
                </Button>
                <Button type="button" disabled={!s.name.trim() || s.creating} onClick={() => void continueFromSources()}>
                  Continue →
                </Button>
              </>
            )}
            {s.step === "image" && phaseA && (
              <>
                <Button type="button" variant="ghost" onClick={goToSources}>
                  Back
                </Button>
                <Button type="button" onClick={() => patch({ partial: true })}>
                  {anyFailed ? "Continue anyway" : "Continue without waiting"}
                </Button>
              </>
            )}
            {s.step === "image" && !phaseA && (
              <>
                <Button type="button" variant="ghost" onClick={goToSources}>
                  Back
                </Button>
                {/* ponytail: the chosen base image stays wizard-local state for
                    this pass — updateWorkspace's client type doesn't accept
                    base_image yet (only createWorkspace was widened for it),
                    and a multi-source workspace's legacy kind/source mirror
                    fields are blank, so calling updateWorkspace here would
                    send garbage rather than the real choice. Persisting it is
                    a small follow-up once that client function grows the same
                    base_image field createWorkspace already has. */}
                <Button type="button" onClick={() => patch({ step: "reqs" })}>
                  Continue →
                </Button>
              </>
            )}
            {s.step === "reqs" && (
              <>
                <Button type="button" variant="outline" onClick={close}>
                  Close
                </Button>
                <Button type="button" disabled={s.savingRequirements} onClick={() => void acceptAndFinish()}>
                  Accept &amp; finish
                </Button>
              </>
            )}
            {s.step === "done" &&
              (origin === "run" ? (
                <Button type="button" onClick={() => (s.workspace ? onAttach?.(s.workspace.id) : close())}>
                  Attach to this run
                </Button>
              ) : origin === "setup" ? (
                <Button type="button" onClick={close}>
                  Back to setup
                </Button>
              ) : (
                <Button
                  type="button"
                  onClick={() => (s.workspace ? (onOpenWorkspace?.(s.workspace.id) ?? close()) : close())}
                >
                  Open {s.name || s.workspace?.name || "workspace"} →
                </Button>
              ))}
          </div>
          {s.step === "image" && phaseA && (
            <p className="w-full text-right text-[0.6875rem] text-muted-foreground">{V2C.WAIT_NOTE}</p>
          )}
          {wsExists && s.step !== "done" && (
            <p className="w-full text-right text-[0.6875rem] text-muted-foreground">{C.CLOSE_KEEPS}</p>
          )}
        </DialogFooter>
      </DialogContent>

      <AlertDialog open={s.confirmBackToSources} onOpenChange={(o) => !o && patch({ confirmBackToSources: false })}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Go back to Sources?</AlertDialogTitle>
            <AlertDialogDescription>{C.RESCAN_DESTROYS}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={confirmGoToSources}>Go back</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Dialog>
  );
}
