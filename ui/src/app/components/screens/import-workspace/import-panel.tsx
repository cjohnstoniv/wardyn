/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ImportWorkspaceDialog — the guided, resumable "import a workspace" overlay
// (Devin-style): Source → Scan → Configure → Record. It clones the
// NewRunDialog pattern: its OWN Dialog on top of Getting Started, its own step
// state, reset-on-open, and it returns to the caller via onOpenChange(false) +
// onReload (never a route change).
//
// Interim panel: this stays the working add-workspace UI until a later wave
// rebuilds Verify/Finalize (import-types.ts keeps the retired verify-checklist
// helpers, unused, for that wave). Record's "Done" just closes the panel —
// there's no separate build+verify or emit-env-as-code step here anymore.
// Env-as-code generation still exists standalone via api.getEnvAsCode (e.g.
// the Workspaces screen's kebab menu) — it never depended on this panel.
//
// Reuse-heavy by design: AddWorkspaceDialog (Source), WorkspaceNeedsPanel
// (Scan/Configure profile + egress-approve), AddSecretDialog (Secrets),
// StepIndicator (rail), usePoll (watch status while scanning, or a legacy
// building/verifying row).
import * as React from "react";
import { CircleCheck, Loader2, Plus, RotateCw, ScanSearch, ShieldCheck, TriangleAlert } from "lucide-react";
import { toast } from "sonner";
import type { Workspace } from "../../../lib/types";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import { runs as runsApi } from "../../../lib/api/runs";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { setup as setupApi } from "../../../lib/api/setup";
import { usePoll } from "../../../lib/use-poll";
import { getErrorMessage as msg } from "../../../lib/format";
import { getDefaultCc } from "../../wardyn/default-confinement";
import { hasLlmPath } from "../onboarding/intro";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "../../ui/dialog";
import { ConfinementChip, SectionLabel } from "../../wardyn/primitives";
import { ConfirmEgressDialog } from "../../wardyn/confirm-egress-dialog";
import { StepIndicator, OptionCard } from "../new-run/step-shell";
import { AddWorkspaceDialog } from "../workspaces";
import { WorkspaceNeedsPanel } from "../workspace-needs-panel";
import { AddSecretDialog } from "../secrets";
import { ProfileReview } from "../profile-review";
import { RecordPane } from "./record-pane";
import {
  IMPORT_STEPS,
  activeStepForStatus,
  isTransientStatus,
  isRecording,
  recordSessions,
  sessionKeyOf,
  newEgressHosts,
  type ImportStepId,
} from "./import-types";

export function ImportWorkspaceDialog({
  open,
  onOpenChange,
  workspaceId,
  onReload,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  // Resume an in-flight import when set; otherwise the panel opens on Source to
  // create/pick a workspace first.
  workspaceId?: string;
  // Called on finish so the caller (Getting Started) re-reads its workspace list.
  onReload: () => void;
}) {
  const [step, setStep] = React.useState<ImportStepId>("source");
  const [wsId, setWsId] = React.useState<string | undefined>(workspaceId);
  const [ws, setWs] = React.useState<Workspace | null>(null);
  const [loadError, setLoadError] = React.useState<string | null>(null);

  // Source-step data.
  const [existing, setExisting] = React.useState<Workspace[]>([]);
  const [addOpen, setAddOpen] = React.useState(false);

  // Brokered secret names (for the Configure step's "already stored" chips).
  const [secretNames, setSecretNames] = React.useState<string[]>([]);
  // fix: Record's "no model configured" warning must be composer-INDEPENDENT —
  // driving it off composer detection fired the warning even with a connected
  // subscription or a stored provider key (a run can have model access with no
  // composer backend). Derive it from GET /setup/status via hasLlmPath instead —
  // the same readiness check Getting Started uses.
  const [llmReady, setLlmReady] = React.useState(false);
  const [addSecretOpen, setAddSecretOpen] = React.useState(false);
  const [addSecretName, setAddSecretName] = React.useState("");

  // Scan busy state + the server's scan-failure detail (ScanPane names the real cause).
  const [scanning, setScanning] = React.useState(false);
  const [scanError, setScanError] = React.useState<string | null>(null);

  // Record step: which task's record run is being kicked, its inline notice
  // (422/503/409), and the ProfileReview drawer's run id ("Save task profile"
  // opens it — the panel owns the drawer so the pane stays free of the profile
  // round-trip).
  const [recordBusyTask, setRecordBusyTask] = React.useState<string | null>(null);
  // The Record step has two modes: record an OPEN session (learn what the task
  // reaches), then replay it CONFINED (prove the approved set is enough).
  const [replayMode, setReplayMode] = React.useState(false);
  const [recordNotice, setRecordNotice] = React.useState<{ status: number; detail?: string } | null>(null);
  const [profileRunId, setProfileRunId] = React.useState<string | null>(null);
  // Suggested "save as is" policy name (workspace + recording) for the profile drawer.
  const [profileName, setProfileName] = React.useState<string | undefined>(undefined);
  // fix: the Record pane's one-click (approveHost) and bulk (promoteEgress)
  // egress approvals used to PUT straight to the API — skipping the same
  // untrusted-content confirm the Workspaces screen enforces for the identical
  // action (the host names come from a workspace's own files or a run's
  // observed egress, neither of which is trusted). Route both through one
  // pending-confirm gate so every caller gets it for free.
  const [pendingConfirm, setPendingConfirm] = React.useState<{ hosts: string[]; run: () => void } | null>(
    null,
  );
  const openProfile = (id: string, name?: string) => {
    setProfileRunId(id);
    setProfileName(name);
  };

  // Guards a one-time auto-scan per workspace so re-renders don't re-fire it.
  const scanFired = React.useRef<string | null>(null);

  const loadWs = React.useCallback(async (id: string, jump = false): Promise<Workspace | undefined> => {
    try {
      const w = await workspacesApi.getWorkspace(id);
      if (!w) {
        setLoadError("Workspace not found.");
        return undefined;
      }
      setLoadError(null);
      setWs(w);
      // A workspace mid-recording resumes on Record (record adds no WorkspaceStatus,
      // so activeStepForStatus can't route to it); otherwise resume by status.
      if (jump) setStep(isRecording(w) ? "record" : activeStepForStatus(w.status));
      return w;
    } catch (e) {
      setLoadError(msg(e));
      return undefined;
    }
  }, []);

  const loadSecrets = React.useCallback(() => {
    secretsApi.listSecrets().then(setSecretNames).catch(() => setSecretNames([]));
  }, []);

  // fix: Record's model-readiness warning (see llmReady above).
  const loadLlmReadiness = React.useCallback(() => {
    setupApi
      .getSetupStatus()
      .then((s) => setLlmReady(hasLlmPath(s)))
      .catch(() => setLlmReady(false));
  }, []);

  // Reset all transient state on each open; resume from the workspace's status
  // when a workspaceId is handed in.
  React.useEffect(() => {
    if (!open) return;
    setWsId(workspaceId);
    setWs(null);
    setLoadError(null);
    setExisting([]);
    setAddOpen(false);
    setAddSecretOpen(false);
    setAddSecretName("");
    setScanning(false);
    setRecordBusyTask(null);
    setRecordNotice(null);
    setProfileRunId(null);
    scanFired.current = null;
    loadSecrets();
    loadLlmReadiness();
    if (workspaceId) {
      setStep("scan"); // provisional; refined by loadWs once the status is known
      void loadWs(workspaceId, true);
    } else {
      setStep("source");
      workspacesApi.listWorkspaces().then(setExisting).catch(() => setExisting([]));
    }
  }, [open, workspaceId, loadWs, loadSecrets, loadLlmReadiness]);

  // Poll the single workspace while it's mid-flight server-side (scanning, or a
  // legacy building/verifying row) — paused otherwise so a settled workspace
  // isn't polled. Record adds NO transient WorkspaceStatus (the status stays
  // `scanned`), so the gate must ALSO stay open while any task is recording, or
  // the pane never updates.
  const transient = !!ws && isTransientStatus(ws.status);
  const recording = !!ws && isRecording(ws);
  usePoll(
    () => {
      if (wsId) void loadWs(wsId);
    },
    2000,
    !(open && !!wsId && (transient || recording)),
  );

  // Auto-scan a freshly created/selected pending_scan workspace once we land on
  // the Scan step (mirrors the setup screen's onSaved auto-scan).
  React.useEffect(() => {
    if (!open || step !== "scan" || !wsId || !ws) return;
    if (ws.status === "pending_scan" && scanFired.current !== wsId) {
      scanFired.current = wsId;
      void doScan();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, step, wsId, ws?.status]);

  const doScan = async () => {
    if (!wsId) return;
    setScanning(true);
    setScanError(null);
    try {
      const { async: isAsync } = await workspacesApi.scanWorkspace(wsId);
      if (isAsync) {
        toast.info("Scanning repo…", {
          description: "A governed scan run is analyzing the repo; the status updates when it completes.",
        });
      }
    } catch (e) {
      // Keep the server's reason: the toast is transient, but ScanPane renders the
      // failure until it's rescanned — it must state the REAL cause, not a guess.
      setScanError(msg(e));
      toast.error("Scan failed", { description: msg(e) });
    } finally {
      setScanning(false);
      await loadWs(wsId); // authoritative status (local dirs scan inline)
    }
  };

  // Select/create a workspace on the Source step → jump into the flow.
  const pickWorkspace = (w: Workspace) => {
    setWsId(w.id);
    setWs(w);
    scanFired.current = null;
    setStep(activeStepForStatus(w.status));
  };

  // Start (or re-start) a NAMED recording session. 400/503/409 render inline;
  // any other failure toasts. Reload after so the pane picks up the `recording`
  // state (and the poll gate stays open while it runs). busyTask keys off the
  // predicted slug so a re-record disables the right card.
  // `confined` REPLAYS a recorded session under the approved set (default-deny
  // egress, off-policy hosts escalate to a live approval) — the least-privilege
  // proof that survived the verify pipeline's retirement. It is keyed
  // verify:<slug> server-side (the record store's own prefix), so the busy key
  // must match or the wrong card spins.
  const doRecord = async (name: string, confined = false) => {
    if (!wsId) return;
    setRecordBusyTask((confined ? "verify:" : "") + sessionKeyOf(name));
    setRecordNotice(null);
    try {
      const r = await workspacesApi.recordTask(wsId, name, confined);
      if (!r.ok) setRecordNotice({ status: r.status, detail: r.detail });
      await loadWs(wsId);
    } catch (e) {
      toast.error(confined ? "Confined replay failed to start" : "Recording failed to start", { description: msg(e) });
    } finally {
      setRecordBusyTask(null);
    }
  };

  // Interactive "Done recording" — kill the run; the backend captures on
  // termination and reconcile flips the record status, which the poll picks up.
  const doneRecording = async (runId: string) => {
    try {
      await runsApi.killRun(runId);
      if (wsId) await loadWs(wsId);
    } catch (e) {
      toast.error("Failed to stop recording", { description: msg(e) });
    }
  };

  // Promote a task's observed egress into the workspace allowlist. Passes the
  // full desired list (approved ∪ observed) so the api's 404 fallback can merge
  // client-side via setApprovedEgress; adopt the returned workspace either way.
  const promoteEgress = async (taskKey: string) => {
    if (!wsId || !ws) return;
    const fallback = [...(ws.approved_egress ?? []), ...newEgressHosts(ws, taskKey)];
    try {
      const updated = await workspacesApi.promoteRecordEgress(wsId, taskKey, fallback);
      setWs(updated);
      toast.success("Approved observed egress");
    } catch (e) {
      toast.error("Failed to promote egress", { description: msg(e) });
    }
  };

  const approveHost = async (host: string) => {
    if (!wsId) return;
    try {
      const updated = await workspacesApi.setApprovedEgress(wsId, [...(ws?.approved_egress ?? []), host]);
      setWs(updated);
      toast.success(`Approved egress to ${host}`);
    } catch (e) {
      toast.error("Failed to approve host", { description: msg(e) });
    }
  };

  // fix: gate both the single-host and bulk approve actions behind the
  // same untrusted-content confirm the Workspaces screen already enforces
  // (ConfirmEgressDialog below) — these wrappers are what Record gets as
  // onApproveHost/onPromoteEgress; the raw approveHost/promoteEgress above only
  // ever run after the operator confirms.
  const requestApproveHost = (host: string) => setPendingConfirm({ hosts: [host], run: () => void approveHost(host) });
  const requestPromoteEgress = (taskKey: string) => {
    if (!ws) return;
    setPendingConfirm({ hosts: newEgressHosts(ws, taskKey), run: () => void promoteEgress(taskKey) });
  };

  // Kill any in-flight interactive record run when the panel closes, so an
  // abandoned attach sandbox doesn't linger until its idle cap. Best-effort +
  // fire-and-forget; the kill ALSO captures the recording server-side
  // (reconcileRecordRun), so closing the panel is an implicit "Done" rather
  // than a lost session.
  const killInFlightRecordings = React.useCallback(() => {
    for (const v of Object.values(ws?.record_results ?? {})) {
      if (v.status === "recording") void runsApi.killRun(v.run_id).catch(() => {});
    }
  }, [ws]);

  const handleClose = React.useCallback(
    (o: boolean) => {
      if (!o) killInFlightRecordings();
      onOpenChange(o);
    },
    [killInFlightRecordings, onOpenChange],
  );

  const finishAndClose = () => {
    killInFlightRecordings();
    onReload();
    onOpenChange(false);
  };

  // Plain client-side finish — no server round-trip. The old Verify/Finalize
  // steps (build+verify, then emit-env-as-code) are retired from this interim
  // panel; env-as-code generation still exists standalone via api.getEnvAsCode
  // (see the Workspaces screen's kebab menu). Reachable from Configure (finish
  // without ever recording anything) or from Record.
  const handleDone = () => {
    toast.success("Workspace added");
    finishAndClose();
  };

  const openAddSecret = (name: string) => {
    setAddSecretName(name);
    setAddSecretOpen(true);
  };

  const stepIdx = IMPORT_STEPS.findIndex((s) => s.id === step);
  const goBack = () => stepIdx > 0 && setStep(IMPORT_STEPS[stepIdx - 1].id);
  const scanned = !!ws && ws.status !== "pending_scan" && ws.status !== "scanning";

  return (
    <>
      <Dialog open={open} onOpenChange={handleClose}>
        <DialogContent className="flex max-h-[88vh] flex-col gap-0 sm:max-w-2xl lg:max-w-4xl">
          <DialogHeader className="border-b border-border pb-4">
            <DialogTitle>Import a workspace</DialogTitle>
            <DialogDescription>{STEP_BLURB[step]}</DialogDescription>
            <div className="pt-3">
              {/* Rail only lets you jump BACKWARD (StepIndicator gates i <= current). */}
              <StepIndicator<ImportStepId> current={step} steps={IMPORT_STEPS} onJump={setStep} />
            </div>
          </DialogHeader>

          <div className="scroll-thin -mx-1 flex-1 overflow-y-auto px-1 py-4">
            {loadError && (
              <div className="mb-3 flex items-start gap-2 rounded-lg border border-danger/30 bg-danger-subtle px-3 py-2 text-xs text-danger">
                <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
                <span>{loadError}</span>
              </div>
            )}

            {step === "source" && (
              <SourcePane existing={existing} onAdd={() => setAddOpen(true)} onPick={pickWorkspace} />
            )}

            {step === "scan" && (
              <ScanPane
                ws={ws}
                scanning={scanning}
                scanError={scanError}
                onRescan={doScan}
                onWorkspaceUpdated={setWs}
              />
            )}

            {step === "configure" && ws && (
              <ConfigurePane
                ws={ws}
                secretNames={secretNames}
                onWorkspaceUpdated={setWs}
                onAddSecret={openAddSecret}
              />
            )}

            {step === "record" && ws && (
              <RecordPane
                ws={ws}
                confined={replayMode}
                notice={recordNotice}
                busyTask={recordBusyTask}
                modelReady={llmReady}
                onRecord={(name: string) => doRecord(name, replayMode)}
                onDoneRecording={doneRecording}
                onPromoteEgress={requestPromoteEgress}
                onApproveHost={requestApproveHost}
                onOpenProfile={openProfile}
              />
            )}
          </div>

          <div className="flex items-center justify-between border-t border-border pt-4">
            <Button variant="ghost" onClick={goBack} disabled={stepIdx === 0}>
              Back
            </Button>
            {/* Forward nav for the steps whose primary action isn't the "advance"
                action itself (Source advances on pick). Configure and Record both
                offer Done — recording is recommended, never required. */}
            {step === "scan" && (
              <Button onClick={() => setStep("configure")} disabled={!scanned}>
                Next: Configure
              </Button>
            )}
            {step === "configure" && (
              <div className="flex items-center gap-2">
                <Button variant="outline" onClick={() => setStep("record")}>
                  Next: Record
                </Button>
                <Button onClick={handleDone}>Done</Button>
              </div>
            )}
            {step === "record" && (
              <div className="flex items-center gap-2">
                {/* Replay confined is the least-privilege proof: re-run a recorded
                    session with ONLY the approved set, so an off-policy host shows
                    up as a live decision instead of a silent allow. It needs a
                    recording to replay, so it stays disabled until there is one. */}
                <Button
                  variant="outline"
                  onClick={() => setReplayMode((r) => !r)}
                  disabled={!replayMode && recordSessions(ws ?? ({} as never), false).length === 0}
                >
                  {replayMode ? "Back to recording" : "Replay confined"}
                </Button>
                <Button onClick={handleDone}>Done</Button>
              </div>
            )}
          </div>
        </DialogContent>
      </Dialog>

      <AddWorkspaceDialog
        open={addOpen}
        onOpenChange={setAddOpen}
        onSaved={(w) => {
          setStep("scan");
          pickWorkspace(w); // pickWorkspace jumps by status; a fresh one is pending_scan => scan
        }}
      />

      <AddSecretDialog
        open={addSecretOpen}
        onOpenChange={setAddSecretOpen}
        existingNames={secretNames}
        initialName={addSecretName}
        onSaved={() => {
          setAddSecretOpen(false);
          loadSecrets();
        }}
      />

      {/* "Save task profile" from a record review card → the existing
          Recording-Mode profile drawer, on the record run's id. */}
      <ProfileReview
        runId={profileRunId}
        suggestedName={profileName}
        onClose={() => setProfileRunId(null)}
      />

      {/* the same untrusted-content confirm Workspaces enforces, gating
          Record's one-click (requestApproveHost) and bulk (requestPromoteEgress)
          egress approvals above. */}
      <ConfirmEgressDialog
        hosts={pendingConfirm?.hosts ?? null}
        onOpenChange={(o) => !o && setPendingConfirm(null)}
        onConfirm={() => {
          const run = pendingConfirm?.run;
          setPendingConfirm(null);
          run?.();
        }}
      />
    </>
  );
}

const STEP_BLURB: Record<ImportStepId, string> = {
  source: "Pick a directory or repo to import. Wardyn scans it once, you review what it needs, then verify the environment before any run touches it.",
  scan: "Wardyn scans the committed files deterministically — languages, package managers, declared secrets (names only), and egress hosts.",
  configure: "Broker any secrets it needs by name, and approve the egress hosts it may reach. Nothing here is agent-authored.",
  record: "Optional but recommended: run each task once in an OPEN recording sandbox to learn what it actually uses, then promote those needs. Skippable — click Done when you're finished.",
};

// ------------------------------------------------------------
// Persistent security chip — active default tier + the brokering guarantee.
// ------------------------------------------------------------
function SecurityChip() {
  const tier = getDefaultCc() ?? "CC1";
  return (
    <div className="flex flex-wrap items-center gap-2 rounded-lg border border-border bg-surface-2/60 px-3 py-2">
      <ShieldCheck className="size-4 shrink-0 text-primary" aria-hidden="true" />
      <ConfinementChip value={tier} />
      <span className="text-xs text-muted-foreground">
        Your default barrier — change it in Getting started, or per run in New Run. Secrets are brokered —
        never written into the sandbox.
      </span>
    </div>
  );
}

// ------------------------------------------------------------
// Source
// ------------------------------------------------------------
function SourcePane({
  existing,
  onAdd,
  onPick,
}: {
  existing: Workspace[];
  onAdd: () => void;
  onPick: (w: Workspace) => void;
}) {
  // Anything not fully "ready" is a resumable import candidate.
  const resumable = existing.filter((w) => w.status !== "ready");
  return (
    <div className="space-y-5">
      <Button onClick={onAdd}>
        <Plus className="size-4" /> Add a new workspace
      </Button>

      {resumable.length > 0 && (
        <section className="space-y-2">
          <SectionLabel>Or resume an in-progress import</SectionLabel>
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
            {resumable.map((w) => (
              <OptionCard
                key={w.id}
                selected={false}
                onClick={() => onPick(w)}
                title={<span className="truncate">{w.name}</span>}
                hint={
                  <span className="truncate">
                    {w.kind === "repo" ? "repo" : "local dir"} · {w.source}
                  </span>
                }
              />
            ))}
          </div>
        </section>
      )}
    </div>
  );
}

// ------------------------------------------------------------
// Scan
// ------------------------------------------------------------
function ScanPane({
  ws,
  scanning,
  scanError,
  onRescan,
  onWorkspaceUpdated,
}: {
  ws: Workspace | null;
  scanning: boolean;
  scanError: string | null;
  onRescan: () => void;
  onWorkspaceUpdated: (w: Workspace) => void;
}) {
  const inFlight = scanning || !ws || ws.status === "pending_scan" || ws.status === "scanning";
  if (inFlight) {
    const isRepo = ws?.kind === "repo";
    return (
      <div className="flex flex-col items-center justify-center gap-3 py-14 text-center">
        <Loader2 className="size-6 animate-spin text-primary" />
        <p className="text-sm font-medium text-foreground">
          {isRepo ? "Cloning the repo and scanning it…" : "Scanning the workspace…"}
        </p>
        <p className="max-w-md text-xs leading-relaxed text-muted-foreground">
          {isRepo
            ? "A governed, sandboxed scan run clones the repo and reads its committed files — detecting languages, package managers, declared secret names (never values), and the egress hosts a build would need. Usually a few seconds."
            : "Reading committed files — detecting languages, package managers, declared secret names (never values), and the egress hosts a build would need."}
        </p>
        {isRepo && (
          <p className="text-[0.6875rem] text-muted-foreground">
            Runs as a real confined run — watch it in <span className="font-medium">Runs</span>, or it clears
            here the moment it finishes.
          </p>
        )}
      </div>
    );
  }
  if (ws.status === "error") {
    return (
      <div className="space-y-3">
        {/* The server's 422 detail is the ONLY thing that names the actual cause
            (e.g. "local directory not found on this host: /home/…"). It used to be
            toasted and then lost, leaving this pane asserting a private-repo
            credential problem for EVERY failure — which sent operators off to add a
            git-pat secret they didn't need. Lead with the real reason; keep the
            credential hint as secondary guidance, and only for repos. */}
        <div className="flex items-start gap-2 rounded-lg border border-danger/30 bg-danger-subtle px-3 py-2.5 text-xs text-danger">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <span>
            {scanError ? (
              <>
                The scan failed: <span className="font-mono">{scanError}</span>
              </>
            ) : (
              <>The scan failed for this workspace. Check the source path/repo, then rescan.</>
            )}
            {ws.kind === "repo" && (
              <>
                {" "}
                A private repo needs a brokered <code className="font-mono">git-pat-&lt;host&gt;</code>{" "}
                or <code className="font-mono">ssh-key-&lt;host&gt;</code> secret before the scan can
                clone it — add one under Secrets (or the SCM Provider setup step) first.
              </>
            )}
          </span>
        </div>
        <Button variant="outline" size="sm" onClick={onRescan}>
          <RotateCw className="size-3.5" /> Rescan
        </Button>
      </div>
    );
  }
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <p className="flex items-center gap-1.5 text-sm text-success">
          <CircleCheck className="size-4" /> Scan complete
        </p>
        <Button variant="ghost" size="sm" onClick={onRescan}>
          <ScanSearch className="size-3.5" /> Rescan
        </Button>
      </div>
      <WorkspaceNeedsPanel workspace={ws} onWorkspaceUpdated={onWorkspaceUpdated} />
    </div>
  );
}

// ------------------------------------------------------------
// Configure
// ------------------------------------------------------------
function ConfigurePane({
  ws,
  secretNames,
  onWorkspaceUpdated,
  onAddSecret,
}: {
  ws: Workspace;
  secretNames: string[];
  onWorkspaceUpdated: (w: Workspace) => void;
  onAddSecret: (name: string) => void;
}) {
  return (
    <div className="space-y-5">
      <SecurityChip />
      <section className="space-y-2">
        <SectionLabel>Detected profile, secrets & egress</SectionLabel>
        {/* Reuses the /workspaces needs panel — declared secrets (names only, with
            an inline Add per un-brokered one), egress tiers, and the
            suggested/observed approve flow — all idempotent server-side. */}
        <WorkspaceNeedsPanel
          workspace={ws}
          onWorkspaceUpdated={onWorkspaceUpdated}
          onAddSecret={onAddSecret}
          brokeredSecretNames={secretNames}
        />
      </section>
    </div>
  );
}
