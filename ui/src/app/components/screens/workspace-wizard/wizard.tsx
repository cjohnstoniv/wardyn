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
import type { SetupStatus, Source as SdkSource, Workspace, WorkspaceProfile } from "../../../lib/types";
import { BUILD_BLURB, StepBuild } from "./step-build";
import { StepIntegrations, INTEGRATIONS_BLURB } from "./step-integrations";
import { VerifyBody } from "./step-requirements";
import { WizardVerifySession } from "./verify-session";
import type { WorkspaceBuildState } from "../../../lib/api/workspaces";
import { StepSources } from "./step-sources";
import { StepBaseImage } from "./step-base-image";
import { StepRequirements } from "./step-requirements";
import { StepDone, type DoneVariant } from "./step-done";
import {
  isFixtureLeak,
  WIZARD_STEPS,
  baseImageStateFromWorkspace,
  canDriveClaudeCode,
  defaultBaseImageState,
  deriveInitialRequirements,
  initialStepFor,
  isSshRemote,
  newSourceRow,
  parseRepoSource,
  removeSource,
  seedFloor,
  sourceRowsFromWorkspace,
  suggestedRegistryImage,
  toBaseImageInput,
  toSourceInput,
  type BaseImageState,
  type PowerSource,
  type SourceRow,
  type SourceScanState,
  type WizardOrigin,
  type WizardStepId,
  type WorkspaceRequirementsMap,
  type WorkspaceSourceInput,
  type WorkspaceSourceKind,
  setRequirementLane,
} from "./wizard-types";

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

interface WizardState {
  step: WizardStepId;
  name: string;
  sources: SourceRow[];
  // The exact wire-shape sources array this wizard was hydrated with (edit
  // mode only) — nulled the instant the operator touches sources, and
  // always null for a fresh create flow. A no-edit save sends this VERBATIM
  // instead of re-deriving through toSourceInput, which can invent a
  // concrete target ("/home/agent/work") for a row whose stored target was
  // "" — turning a click-through-with-zero-edits save into a spurious
  // sourcesChanged on the server that wipes the reviewed contract
  // (Requirements/Profile/ApprovedEgress). NEVER omit `sources` from a PUT
  // to "leave it unchanged" instead — an absent sources array decodes
  // server-side to a single ephemeral source, which is catastrophic, not a
  // no-op.
  initialSources: WorkspaceSourceInput[] | null;
  secretNames: string[];
  githubApp: boolean;
  // The whole setup status, kept so the requirements step can offer the
  // integrations a workspace may name. null until the fetch lands.
  setupStatus: SetupStatus | null;
  harnessAvailable: boolean;
  workspace: Workspace | null;
  scans: Record<string, SourceScanState>;
  scanning: boolean;
  partial: boolean;
  baseImage: BaseImageState;

  requirements: WorkspaceRequirementsMap;
  requirementsSeeded: boolean;
  confirmBackToSources: boolean;
  creating: boolean;
  savingImage: boolean;
  savingRequirements: boolean;
  buildState: WorkspaceBuildState | null;
}

// initial, when given, hydrates the wizard onto an ALREADY-onboarded
// workspace (the "Edit workspace…" entry point) instead of a blank create
// flow: workspace is seeded so continueFromSources's create-branch (POST)
// never fires, sources/baseImage/requirements seed from the row, and the
// wizard lands on whatever step this workspace hasn't cleared yet
// (initialStepFor) instead of always starting at Sources.
function initialState(initial?: Workspace): WizardState {
  return {
    step: initial ? initialStepFor(initial) : "sources",
    name: initial?.name ?? "",
    sources: initial ? sourceRowsFromWorkspace(initial) : seedFloor(),
    initialSources: initial?.sources ?? null,
    secretNames: [],
    githubApp: false,
    setupStatus: null,
    harnessAvailable: false,
    workspace: initial ?? null,
    scans: {},
    scanning: false,
    partial: false,
    baseImage: initial ? baseImageStateFromWorkspace(initial) : defaultBaseImageState(),

    requirements: initial?.requirements ?? {},
    requirementsSeeded: false,
    confirmBackToSources: false,
    creating: false,
    savingImage: false,
    savingRequirements: false,
    buildState: null,
  };
}

function profileOf(ws: Workspace | null): WorkspaceProfile | null {
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
  initial,
  onClose,
  onWorkspaceCreated,
  onOpenWorkspace,
  onAttach,
}: {
  origin?: WizardOrigin;
  /** Hydrates the wizard onto an ALREADY-onboarded workspace instead of a
   *  blank create flow — the "Edit workspace…" entry point (workspaces.tsx /
   *  workspace-detail.tsx). Read once at mount (the lazy useState initializer
   *  below); callers remount the wizard (a fresh `key`) to edit a different row. */
  initial?: Workspace;
  onClose: () => void;
  /** Fired once the workspace is created, so a host list can refresh. */
  onWorkspaceCreated?: (workspace: Workspace) => void;
  /** Done's "Make it stronger" cards deep-link here — wiring the real route is
   *  a later step (this wizard isn't mounted into the app yet); omitted, the
   *  cards are inert. */
  onOpenWorkspace?: (workspaceId: string, focus?: "record" | "env" | "model") => void;
  /** origin="run" only — omitted, Done's primary action falls back to closing. */
  onAttach?: (workspaceId: string) => void;
}) {
  const isEdit = !!initial;
  const [state, setState] = React.useState<WizardState>(() => initialState(initial));
  const patch = (p: Partial<WizardState>) => setState((s) => ({ ...s, ...p }));
  const s = state;
  // The wizard no longer pins a power source (that control lives on the
  // workspace page); it only READS the resolution, derived from whether any
  // integration can drive this image's agent tool.
  const powerSource: PowerSource = s.harnessAvailable ? { kind: "default" } : { kind: "none" };

  React.useEffect(() => {
    let live = true;
    secretsApi.listSecrets().then((names) => live && patch({ secretNames: names })).catch(() => {});
    setupApi
      .getSetupStatus()
      .then((status) => live && patch({ githubApp: status.secrets.github_app, setupStatus: status }))
      .catch(() => {});
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
  // Every mutator below nulls initialSources — the instant the operator
  // touches sources at all, the "send the verbatim baseline" no-edit path
  // (H2) no longer applies and a fresh toSourceInput derivation takes over.
  const addSource = (type: WorkspaceSourceKind) =>
    patch({ sources: [...s.sources, newSourceRow(type)], initialSources: null });
  // Attach a tier-1 LIBRARY entry: resolve it into a prefilled row. The server
  // upsert dedupes on canonical identity, so create lands the attachment on
  // the SAME library row — contract, scan and all.
  const attachLibrarySource = (src: SdkSource) =>
    patch({
      sources: [
        ...s.sources,
        {
          ...newSourceRow(src.kind === "repo" ? "repo" : "local_dir"),
          path: src.kind === "local_dir" ? src.locator : "",
          source: src.kind === "repo" ? src.locator : "",
          ref: src.ref ?? "",
        },
      ],
      initialSources: null,
    });
  const updateSource = (id: string, p: Partial<SourceRow>) =>
    patch({ sources: s.sources.map((r) => (r.id === id ? { ...r, ...p } : r)), initialSources: null });
  const removeSourceRow = (id: string) => patch({ sources: removeSource(s.sources, id), initialSources: null });
  const onSecretStored = (name: string) => patch({ secretNames: [...s.secretNames, name] });

  // ---------- Scan orchestration (Sources -> Base image) ----------
  // ponytail: POST /workspaces/{id}/scan scans the whole composed workspace in
  // one call — there is no per-source scan endpoint yet. Per-source rows still
  // render independently (an SSH-gated row fails immediately, client-side,
  // without waiting on the network), but every OTHER non-ephemeral row shares
  // the one real outcome. Good enough for the wizard's UI; a true per-source
  // signal is a backend addition for later, not a UI-only gap to paper over
  // with invented per-source state.
  const startScan = async (ws: Workspace) => {
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
        finalWs = (await workspacesApi.getWorkspace(ws.id)) ?? ws;
      } else {
        for (let i = 0; i < 40; i++) {
          await sleep(1500);
          const polled = await workspacesApi.getWorkspace(ws.id);
          if (polled && polled.status !== "scanning" && polled.status !== "pending_scan") {
            finalWs = polled;
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
      // Edit-mode re-entry (e.g. the operator went back and forward again,
      // or is walking the "Edit workspace…" flow from Sources): PUT the
      // current sources/base_image FIRST, THEN scan the result. The old
      // order scanned the server's stale, unedited composition and PUT the
      // edit only afterward (in continueFromImage) — so that later PUT could
      // detect sourcesChanged against the very scan that just ran, wiping
      // the fresh profile back to null and stranding the rail behind it
      // (the reqs/verify rail steps gate on `profile` being truthy). PUTting
      // first means the scan that follows is against the RIGHT composition.
      // updateWorkspace DOES accept `sources` — always has, same
      // composition-shape PUT continueFromImage below already uses.
      patch({ creating: true });
      try {
        const updated = await workspacesApi.updateWorkspace(s.workspace.id, {
          name: s.name.trim(),
          sources: s.initialSources ?? s.sources.map((r) => toSourceInput(r, s.sources)),
          base_image: toBaseImageInput(s.baseImage, detectedChips),
        });
        patch({ creating: false, workspace: updated });
        void startScan(updated);
      } catch (e) {
        patch({ creating: false });
        toast.error("Failed to save the edited sources", { description: getErrorMessage(e) });
      }
      return;
    }
    patch({ creating: true });
    try {
      const ws = await workspacesApi.createWorkspace({
        name: s.name.trim(),
        sources: s.sources.map((r) => toSourceInput(r, s.sources)),
      });
      const created = ws;
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
      // An edit session where sources are still exactly the hydrated
      // baseline (initialSources non-null — nothing touched yet) restores
      // the ORIGINAL requirements instead of wiping them: the underlying
      // composition hasn't actually changed, so there is nothing for a
      // rescan to legitimately re-derive, and the old unconditional {} here
      // downgraded every operator_set lane back to scan_seeded defaults (or
      // dropped it entirely) on the very next full-replace requirements PUT.
      // Once sources ARE edited (initialSources nulled), the composition
      // genuinely changed, so the old wipe-and-reseed-from-profile applies.
      requirements: s.initialSources ? (initial?.requirements ?? {}) : {},
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

  // Leaving Requirements SAVES the contract first — the verify session's
  // egress posture is computed server-side from the STORED rows, so an
  // unsaved contract would verify against yesterday's.
  const saveAndContinue = async () => {
    if (!s.workspace) return;
    patch({ savingRequirements: true });
    try {
      const updated = await workspacesApi.setRequirements(s.workspace.id, s.requirements);
      patch({ savingRequirements: false, workspace: updated, step: "verify" });
    } catch (e) {
      patch({ savingRequirements: false });
      toast.error("Failed to save requirements", { description: getErrorMessage(e) });
    }
  };

  // Leaving step ②: PERSIST the base-image choice (it used to be wizard-local
  // and silently thrown away — a picked golang image never reached the
  // workspace, so verify booted the stock agent image and `go` was "command
  // not found"). Image-only updates keep the contract server-side; a failed
  // save stays on the step with the error in a toast.
  const continueFromImage = async () => {
    if (!s.workspace) {
      patch({ step: "integrations" });
      return;
    }
    patch({ savingImage: true });
    try {
      const updated = await workspacesApi.updateWorkspace(s.workspace.id, {
        name: s.name || s.workspace.name,
        // s.initialSources ?? ...: an edit session that reaches this step
        // with sources still untouched sends the VERBATIM hydrated baseline
        // rather than a fresh toSourceInput derivation, which can invent a
        // concrete target ("/home/agent/work") for a row whose stored
        // target was "" — a zero-edit save must not look like a sources
        // edit to the server (sourcesChanged wipes the reviewed contract).
        sources: s.initialSources ?? s.sources.map((r) => toSourceInput(r, s.sources)),
        base_image: toBaseImageInput(s.baseImage, detectedChips),
      });
      patch({ workspace: updated, savingImage: false, step: "integrations" });
    } catch (e) {
      patch({ savingImage: false });
      toast.error("Failed to save the base-image choice", { description: getErrorMessage(e) });
    }
  };

  // After a verify session ends: the decide() hook may have written rows into
  // the WORKSPACE's contract server-side (approved hosts, required/operator_set).
  // Refetch and absorb them — new keys join the wizard's map; a key the
  // operator already edited locally keeps the local value (their unsaved edit
  // wins until Accept & finish PUTs the map).
  const absorbServerContract = async () => {
    if (!s.workspace) return;
    const fresh = await workspacesApi.getWorkspace(s.workspace.id).catch(() => null);
    if (!fresh) return;
    const merged = { ...s.requirements };
    for (const [k, v] of Object.entries(fresh.requirements ?? {})) {
      if (!(k in merged)) merged[k] = v;
    }
    patch({ workspace: fresh, requirements: merged });
  };

  // What a verify session will boot, stated honestly: an explicit image pick
  // boots verbatim; "recommended" boots the BUILT image when one exists, and
  // on a host without devcontainer builds it falls back to the stock agent
  // image — say so up front instead of letting "command not found" say it.
  const carryImage =
    s.baseImage.choice === "catalog" && s.baseImage.catalog
      ? s.baseImage.catalog.image
      : s.baseImage.choice === "byo"
        ? s.baseImage.byoRef.trim() || "bring-your-own — no ref yet"
        : s.baseImage.choice === "registry"
          ? suggestedRegistryImage(detectedChips)
          : s.baseImage.choice === "custom"
            ? s.baseImage.customBase.trim()
            : s.workspace?.image_ref
              ? s.workspace.image_ref
              : "recommended build — built on first use; without devcontainer builds on this host, sessions boot the stock agent image";

  // ---------- Done (step ⑤) ----------
  const doneVariant: DoneVariant = !s.workspace
    ? "scanning"
    : s.workspace.status === "error"
      ? "failed"
      : s.workspace.status === "scanning" || s.workspace.status === "pending_scan"
        ? "scanning"
        : "usable";
  // HOT leaks only: a fixture in a test file must not read as "rotate before
  // mounting" on the Done step (same tier split as the requirements banner).
  const leakCount = (profile?.leak_findings ?? []).filter((l) => !isFixtureLeak(l)).length;

  const wsExists = !!s.workspace;
  const blurb =
    s.step === "sources"
      ? V2C.S1_BLURB
      : s.step === "image"
        ? V2C.S2_BLURB
        : s.step === "integrations"
          ? INTEGRATIONS_BLURB
          : s.step === "build"
            ? BUILD_BLURB
            : s.step === "reqs"
              ? C.S3_BLURB
              : s.step === "verify"
                ? "Drive the workspace for real. Anything not in the contract is held at the door — approve or deny it live, adjust, retry, then finish."
                : undefined;

  // Outside-click and Esc are otherwise indistinguishable from the X button,
  // but most steps have no explicit Close — losing the dialog this way mid-
  // Build (or any step past Sources) used to strand a half-onboarded
  // workspace with no way back. Block them once there's something to strand
  // (same condition as the footer's CLOSE_KEEPS note below); the X stays a
  // deliberate one-click close either way. ponytail: block, don't confirm —
  // add a confirm-on-X only if real usage shows accidental X-clicks too.
  const blockAccidentalDismiss = wsExists && s.step !== "done";

  return (
    <Dialog open onOpenChange={(o) => !o && close()}>
      <DialogContent
        // 5xl: the 7-step rail (numbered circles + always-on labels + rails)
        // needs ~900px on one line — at 2xl steps ⑥⑦ rendered past the panel
        // edge, and 4xl still folded "Done" onto a second row.
        className="flex max-h-[85vh] flex-col gap-0 p-0 sm:max-w-5xl"
        onPointerDownOutside={(e) => blockAccidentalDismiss && e.preventDefault()}
        onEscapeKeyDown={(e) => blockAccidentalDismiss && e.preventDefault()}
      >
        <div className="space-y-2.5 border-b border-border px-6 py-4">
          <DialogHeader>
            <DialogTitle>{isEdit ? "Edit workspace" : "Add workspace"}</DialogTitle>
            {blurb && <DialogDescription>{blurb}</DialogDescription>}
          </DialogHeader>
          <StepIndicator
            steps={WIZARD_STEPS}
            current={s.step}
            onJump={(id) => {
              if (id === "sources") goToSources();
              else if (id === "image" && wsExists) patch({ step: "image" });
              else if (id === "integrations" && wsExists) patch({ step: "integrations" });
              else if (id === "build" && wsExists) patch({ step: "build" });
              else if (id === "reqs" && wsExists && profile) patch({ step: "reqs" });
              else if (id === "verify" && wsExists && profile) patch({ step: "verify" });
            }}
          />
        </div>

        <div className="scroll-thin flex-1 overflow-y-auto px-6 py-5">
          {s.step === "sources" && (
            <StepSources
              onAttachLibrarySource={attachLibrarySource}
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
              state={s.baseImage}
              onChange={(p) => patch({ baseImage: { ...s.baseImage, ...p } })}
            />
          )}
          {s.step === "integrations" && (
            <StepIntegrations
              status={s.setupStatus ?? null}
              requirements={s.requirements}
              setLane={(key, level) => patch({ requirements: setRequirementLane(s.requirements, key, level) })}
              clear={(key) => {
                const next = { ...s.requirements };
                delete next[key];
                patch({ requirements: next });
              }}
            />
          )}
          {s.step === "build" && s.workspace && (
            <StepBuild workspaceId={s.workspace.id} onStateChange={(b) => patch({ buildState: b })} />
          )}
          {s.step === "verify" && s.workspace && (
            <VerifyBody
              requirements={s.requirements}
              storedSecretNames={s.secretNames}
              powerSource={powerSource}
              carryImage={carryImage}
              onOpenReach={() => patch({ step: "reqs" })}
              verifyPanel={
                <WizardVerifySession
                  ws={s.workspace}
                  nothingResolves={powerSource.kind === "none"}
                  onContractChanged={() => void absorbServerContract()}
                />
              }
            />
          )}
          {s.step === "reqs" && (
            <StepRequirements
              carryImage={carryImage}
              showVerifyTab={false}
              profile={profile}
              sources={s.sources}
              requirements={s.requirements}
              onChange={(requirements) => patch({ requirements })}
              storedSecretNames={s.secretNames}
              onSecretStored={onSecretStored}
              powerSource={powerSource}
              // Named-only: the wizard's step ③ OWNS the integrations picker,
              // so Reach shows just the rows the contract names — WITH status,
              // because existence must stay honest (omitting it told the
              // operator an adopted row was "not configured"). The workspace
              // DETAIL page has no step ③, so its Reach keeps the full picker.
              status={s.setupStatus ?? null}
              integrationsNamedOnly
            />
          )}
          {s.step === "done" && (
            <StepDone
              name={s.name || s.workspace?.name || "workspace"}
              variant={doneVariant}
              requirements={s.requirements}
              storedSecretNames={s.secretNames}
              leakCount={leakCount}
              powerSource={powerSource}
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
                <Button type="button" disabled={s.savingImage} onClick={() => void continueFromImage()}>
                  Continue →
                </Button>
              </>
            )}
            {s.step === "integrations" && (
              <>
                <Button type="button" variant="ghost" onClick={() => patch({ step: "image" })}>
                  Back
                </Button>
                <Button type="button" onClick={() => patch({ step: "build" })}>
                  Continue →
                </Button>
              </>
            )}
            {s.step === "build" && (
              <>
                <Button type="button" variant="ghost" onClick={() => patch({ step: "integrations" })}>
                  Back
                </Button>
                {/* Continue never hard-blocks: a failed/absent build falls back
                    to the stock agent image at session time, stated honestly on
                    the Verify step's carry card — but while a build RUNS, moving
                    on just means arriving before the image does. */}
                <Button type="button" onClick={() => patch({ step: "reqs" })}>
                  {s.buildState?.state === "building" ? "Continue without waiting" : "Continue →"}
                </Button>
              </>
            )}
            {s.step === "reqs" && (
              <>
                <Button type="button" variant="ghost" onClick={() => patch({ step: "build" })}>
                  Back
                </Button>
                <Button type="button" disabled={s.savingRequirements} onClick={() => void saveAndContinue()}>
                  Save &amp; continue →
                </Button>
              </>
            )}
            {s.step === "verify" && (
              <>
                <Button type="button" variant="ghost" onClick={() => patch({ step: "reqs" })}>
                  Back
                </Button>
                <Button type="button" onClick={() => patch({ step: "done" })}>
                  Finish
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
          {blockAccidentalDismiss && (
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
