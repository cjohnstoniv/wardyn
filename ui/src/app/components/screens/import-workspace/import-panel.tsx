/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ImportWorkspaceDialog — the guided, resumable "import a workspace" overlay
// (Devin-style): Source → Scan → Configure → Verify → Finalize. It clones the
// NewRunDialog pattern: its OWN Dialog on top of Getting Started, its own step
// state, reset-on-open, and it returns to the caller via onOpenChange(false) +
// onReload (never a route change).
//
// Reuse-heavy by design: AddWorkspaceDialog (Source), WorkspaceNeedsPanel
// (Scan/Configure profile + egress-approve), AddSecretDialog (Secrets),
// StepIndicator (rail), usePoll (watch status while scanning/building/verifying).
import * as React from "react";
import {
  Check,
  CircleCheck,
  CircleX,
  Copy,
  FileCode2,
  Loader2,
  Play,
  Plus,
  RotateCw,
  ScanSearch,
  ShieldCheck,
  Sparkles,
  TriangleAlert,
} from "lucide-react";
import { toast } from "sonner";
import type { SetupCommand, Workspace, WorkspaceProfile } from "../../../lib/types";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import { runs as runsApi } from "../../../lib/api/runs";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { setup as setupApi } from "../../../lib/api/setup";
import { composer as composerApi } from "../../../lib/api/compose";
import { usePoll } from "../../../lib/use-poll";
import { useCopyToClipboard } from "../../../lib/use-copy-to-clipboard";
import { getErrorMessage as msg } from "../../../lib/format";
import { getDefaultCc } from "../../wardyn/default-confinement";
import { hasLlmPath } from "../onboarding/intro";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Checkbox } from "../../ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "../../ui/dialog";
import { Chip, ConfinementChip, SectionLabel } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";
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
  verifyPhase,
  verifyRows,
  verifyProgress,
  runningLabel,
  fmtStepDuration,
  VERIFY_PHASE_LABEL,
  VERIFY_PHASE_TONE,
  type ImportStepId,
  type VerifyRow,
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
  // Whether a composer backend is configured — gates the AGENTIC "diagnose with AI"
  // affordance on the Verify step (detected the same way new-run-dialog does: an
  // empty backend list means the composer is off / suggest-fix would 404).
  const [composerEnabled, setComposerEnabled] = React.useState(false);
  // fix: Record's "no model configured" warning must be composer-INDEPENDENT —
  // driving it off composerEnabled fired the warning even with a connected
  // subscription or a stored provider key (a run can have model access with no
  // composer backend). Derive it from GET /setup/status via hasLlmPath instead —
  // the same readiness check Getting Started uses.
  const [llmReady, setLlmReady] = React.useState(false);
  const [addSecretOpen, setAddSecretOpen] = React.useState(false);
  const [addSecretName, setAddSecretName] = React.useState("");

  // Scan / verify busy + verify inline notice (422/503/409).
  const [scanning, setScanning] = React.useState(false);
  // The server's scan-failure detail, kept so ScanPane can name the real cause.
  const [scanError, setScanError] = React.useState<string | null>(null);
  const [verifyBusy, setVerifyBusy] = React.useState(false);
  const [verifyNotice, setVerifyNotice] = React.useState<{ status: number; detail?: string } | null>(null);

  // Record step: which task's record run is being kicked, its inline notice
  // (422/503/409, same shape as verify), and the ProfileReview drawer's run id
  // ("Save task profile" opens it — the panel owns the drawer so the pane stays
  // free of the profile round-trip).
  const [recordBusyTask, setRecordBusyTask] = React.useState<string | null>(null);
  const [recordNotice, setRecordNotice] = React.useState<{ status: number; detail?: string } | null>(null);
  const [profileRunId, setProfileRunId] = React.useState<string | null>(null);
  // Suggested "save as is" policy name (workspace + recording) for the profile drawer.
  const [profileName, setProfileName] = React.useState<string | undefined>(undefined);
  // fix: the Record/Verify panes' one-click (approveHost) and bulk
  // (promoteEgress) egress approvals used to PUT straight to the API — skipping
  // the same untrusted-content confirm the Workspaces screen enforces for the
  // identical action (the host names come from a workspace's own files or a
  // run's observed egress, neither of which is trusted). Route both through
  // one pending-confirm gate so every caller gets it for free.
  const [pendingConfirm, setPendingConfirm] = React.useState<{ hosts: string[]; run: () => void } | null>(
    null,
  );
  const openProfile = (id: string, name?: string) => {
    setProfileRunId(id);
    setProfileName(name);
  };

  // Finalize state.
  const [emitCode, setEmitCode] = React.useState(false);
  const [finalizing, setFinalizing] = React.useState(false);
  const [emitted, setEmitted] = React.useState<Record<string, string> | null>(null);

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

  const loadComposer = React.useCallback(() => {
    // The "Diagnose with AI" affordance shows only when a composer backend is
    // actually configured (an empty list / probe error hides it, no crash).
    composerApi
      .listComposerBackends()
      .then((bs) => setComposerEnabled(bs.length > 0))
      .catch(() => setComposerEnabled(false));
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
    setVerifyBusy(false);
    setVerifyNotice(null);
    setRecordBusyTask(null);
    setRecordNotice(null);
    setProfileRunId(null);
    setEmitCode(false);
    setFinalizing(false);
    setEmitted(null);
    scanFired.current = null;
    loadSecrets();
    loadComposer();
    loadLlmReadiness();
    if (workspaceId) {
      setStep("scan"); // provisional; refined by loadWs once the status is known
      void loadWs(workspaceId, true);
    } else {
      setStep("source");
      workspacesApi.listWorkspaces().then(setExisting).catch(() => setExisting([]));
    }
  }, [open, workspaceId, loadWs, loadSecrets, loadComposer, loadLlmReadiness]);

  // Poll the single workspace while it's mid-flight server-side (scanning /
  // building / verifying) — paused otherwise so a settled workspace isn't polled.
  // Record adds NO transient WorkspaceStatus (the status stays `scanned`), so the
  // gate must ALSO stay open while any task is recording, or the pane never updates.
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

  const doVerify = async () => {
    if (!wsId) return;
    setVerifyBusy(true);
    setVerifyNotice(null);
    try {
      const r = await workspacesApi.verifyWorkspace(wsId);
      if (!r.ok) setVerifyNotice({ status: r.status, detail: r.detail });
      await loadWs(wsId); // pick up building/verifying/verify_failed
    } catch (e) {
      toast.error("Verification failed to start", { description: msg(e) });
    } finally {
      setVerifyBusy(false);
    }
  };

  // Start (or re-start) a NAMED recording session. 400/503/409 render inline (same
  // as verify); any other failure toasts. Reload after so the pane picks up the
  // `recording` state (and the poll gate stays open while it runs). busyTask keys
  // off the predicted slug so a re-record disables the right card.
  const doRecord = async (name: string, confined = false) => {
    if (!wsId) return;
    // A confined verify run is keyed verify:<slug> server-side (verifyKeyOf) — match
    // that so the right recording's card shows busy.
    setRecordBusyTask((confined ? "verify:" : "") + sessionKeyOf(name));
    setRecordNotice(null);
    try {
      const r = await workspacesApi.recordTask(wsId, name, confined);
      if (!r.ok) setRecordNotice({ status: r.status, detail: r.detail });
      await loadWs(wsId);
    } catch (e) {
      toast.error(confined ? "Verify session failed to start" : "Recording failed to start", { description: msg(e) });
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
  // (ConfirmEgressDialog below) — these wrappers are what Record/Verify get as
  // onApproveHost/onPromoteEgress; the raw approveHost/promoteEgress above only
  // ever run after the operator confirms.
  const requestApproveHost = (host: string) => setPendingConfirm({ hosts: [host], run: () => void approveHost(host) });
  const requestPromoteEgress = (taskKey: string) => {
    if (!ws) return;
    setPendingConfirm({ hosts: newEgressHosts(ws, taskKey), run: () => void promoteEgress(taskKey) });
  };

  const doFinalize = async () => {
    if (!wsId) return;
    setFinalizing(true);
    try {
      const { workspace, emitted_files } = await workspacesApi.finalizeWorkspace(wsId, { emitEnvAsCode: emitCode });
      setWs(workspace);
      if (Object.keys(emitted_files).length) {
        setEmitted(emitted_files); // show the copyable files, then Done closes
        toast.success("Workspace imported");
      } else {
        finishAndClose();
      }
    } catch (e) {
      toast.error("Finalize failed", { description: msg(e) });
    } finally {
      setFinalizing(false);
    }
  };

  // Kill any in-flight interactive record/verify run when the panel closes, so an
  // abandoned attach sandbox doesn't linger until its idle cap. Best-effort + fire-
  // and-forget; the kill ALSO captures the recording server-side (reconcileRecordRun),
  // so closing the panel is an implicit "Done" rather than a lost session.
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
                notice={recordNotice}
                busyTask={recordBusyTask}
                modelReady={llmReady}
                onRecord={(name) => doRecord(name, false)}
                onDoneRecording={doneRecording}
                onPromoteEgress={requestPromoteEgress}
                onApproveHost={requestApproveHost}
                onOpenProfile={openProfile}
              />
            )}

            {/* Verify = re-run your steps in a CONFINED session (default-deny egress,
                limited to the approved set); off-policy hosts are blocked live. The
                older automated setup-command verify (VerifyPane) is still shown when
                an auto-verify is in flight or has a result — driven via the API.
                fix: it's ALSO the fallback when Record was skipped (zero open
                recordings to replay) — otherwise "Skip recording" stranded the
                operator at RecordPane's confined dead-end, contradicting
                STEP_BLURB.record's "Verify still proves it either way". */}
            {step === "verify" && ws &&
              (ws.verify_result ||
              ["building", "build_error", "verifying", "verify_failed"].includes(ws.status) ||
              recordSessions(ws, false).length === 0 ? (
                <VerifyPane
                  ws={ws}
                  busy={verifyBusy}
                  notice={verifyNotice}
                  composerEnabled={composerEnabled}
                  onVerify={doVerify}
                  onApproveHost={requestApproveHost}
                  onBackToConfigure={() => setStep("configure")}
                  onContinueFinalize={() => setStep("finalize")}
                />
              ) : (
                <RecordPane
                  ws={ws}
                  confined
                  notice={recordNotice}
                  busyTask={recordBusyTask}
                  modelReady={llmReady}
                  onRecord={(name) => doRecord(name, true)}
                  onDoneRecording={doneRecording}
                  onPromoteEgress={requestPromoteEgress}
                  onApproveHost={requestApproveHost}
                  onOpenProfile={openProfile}
                />
              ))}

            {step === "finalize" && ws && (
              <FinalizePane
                ws={ws}
                emitCode={emitCode}
                onEmitCodeChange={setEmitCode}
                finalizing={finalizing}
                emitted={emitted}
                onFinalize={doFinalize}
                onDone={finishAndClose}
              />
            )}
          </div>

          <div className="flex items-center justify-between border-t border-border pt-4">
            <Button variant="ghost" onClick={goBack} disabled={stepIdx === 0}>
              Back
            </Button>
            {/* Forward nav for the steps whose primary action isn't the "advance"
                action itself (Source advances on pick; Finalize's action closes). */}
            {step === "scan" && (
              <Button onClick={() => setStep("configure")} disabled={!scanned}>
                Next: Configure
              </Button>
            )}
            {step === "configure" && (
              <Button onClick={() => setStep("record")}>Next: Record</Button>
            )}
            {step === "record" && (
              <div className="flex items-center gap-2">
                {/* Both advance to Verify — recording is recommended, never required. */}
                <Button variant="outline" onClick={() => setStep("verify")}>
                  Skip recording
                </Button>
                <Button onClick={() => setStep("verify")}>Continue to Verify</Button>
              </div>
            )}
            {step === "verify" && (
              <Button variant="outline" onClick={() => setStep("finalize")}>
                Continue to Finalize
              </Button>
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
          Record/Verify's one-click (requestApproveHost) and bulk
          (requestPromoteEgress) egress approvals above. */}
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
  configure: "Approve the detected setup commands, broker any secrets it needs by name, and approve the egress hosts it may reach. Nothing here is agent-authored.",
  record: "Optional but recommended: run each task once in an OPEN recording sandbox to learn what it actually uses, then promote those needs. Skippable — Verify still proves the environment either way.",
  verify: "Run the approved setup commands in a governed sandbox to prove the environment builds before an agent uses it.",
  finalize: "Lock in the reviewed profile. Optionally emit a devcontainer.json / AGENTS.md you can commit.",
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
      <SetupCommandsCard ws={ws} onSaved={onWorkspaceUpdated} />
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

// The closed stage set the server accepts (workspacescan.setupStages) — an
// unlisted stage 400s (validateSetupCommand). Kept in sync by hand; small and
// stable enough that a shared wire contract would be overkill here.
const SETUP_COMMAND_STAGES = ["install", "build", "test", "lint"] as const;

// Approve/edit the scanner-detected setup commands, then PUT them (the approved
// list a verify run executes). Working copy starts from the operator-approved
// list if present, else the detected proposal.
function SetupCommandsCard({ ws, onSaved }: { ws: Workspace; onSaved: (w: Workspace) => void }) {
  const profile = (ws.profile ?? {}) as WorkspaceProfile;
  const detected = React.useMemo(() => profile.setup_commands ?? [], [profile.setup_commands]);
  const approved = React.useMemo(() => ws.setup_commands ?? [], [ws.setup_commands]);

  type Row = SetupCommand & { include: boolean };
  const seed = React.useCallback(
    (): Row[] =>
      (approved.length ? approved : detected).map((c) => ({ ...c, include: true })),
    [approved, detected],
  );
  const [rows, setRows] = React.useState<Row[]>(seed);
  const [saving, setSaving] = React.useState(false);
  // Re-seed when we switch to a different workspace.
  React.useEffect(() => {
    setRows(seed());
  }, [ws.id, seed]);

  const setRow = (i: number, patch: Partial<Row>) =>
    setRows((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const addRow = () =>
    setRows((rs) => [...rs, { stage: "install", command: "", source: "operator", include: true }]);

  const save = async () => {
    setSaving(true);
    try {
      const commands: SetupCommand[] = rows
        .filter((r) => r.include && r.command.trim())
        .map(({ stage, command, source }) => ({ stage, command: command.trim(), source }));
      onSaved(await workspacesApi.setSetupCommands(ws.id, commands));
      toast.success("Setup commands saved");
    } catch (e) {
      toast.error("Failed to save setup commands", { description: msg(e) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="space-y-2.5" data-testid="setup-commands">
      <SectionLabel>Setup commands</SectionLabel>
      <p className="text-[0.6875rem] leading-snug text-muted-foreground">
        Detected from committed files (untrusted). Approve or edit what runs during verify — nothing
        runs until you save.
      </p>
      {rows.length === 0 ? (
        <p className="text-xs text-muted-foreground">No setup commands detected. Add one to verify a build.</p>
      ) : (
        <ul className="space-y-2">
          {rows.map((r, i) => (
            <li key={i} className="flex items-center gap-2 rounded-lg border border-border p-2.5">
              <Checkbox
                checked={r.include}
                onCheckedChange={(v) => setRow(i, { include: !!v })}
                aria-label={`Include ${r.stage} command`}
              />
              <select
                value={r.stage}
                onChange={(e) => setRow(i, { stage: e.target.value })}
                aria-label={`Stage for command ${i + 1}`}
                className="h-8 rounded-md border border-border bg-transparent px-2 text-xs"
              >
                {SETUP_COMMAND_STAGES.map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </select>
              <Input
                value={r.command}
                onChange={(e) => setRow(i, { command: e.target.value })}
                className="h-8 flex-1 font-mono text-xs"
                placeholder="npm ci"
              />
            </li>
          ))}
        </ul>
      )}
      <div className="flex items-center gap-2">
        <Button variant="outline" size="sm" onClick={addRow}>
          <Plus className="size-3.5" /> Add command
        </Button>
        <Button size="sm" onClick={save} disabled={saving}>
          {saving ? <Loader2 className="size-3.5 animate-spin" /> : <Check className="size-3.5" />}
          Save setup commands
        </Button>
      </div>
    </section>
  );
}

// ------------------------------------------------------------
// Verify
// ------------------------------------------------------------
function VerifyPane({
  ws,
  busy,
  notice,
  composerEnabled,
  onVerify,
  onApproveHost,
  onBackToConfigure,
  onContinueFinalize,
}: {
  ws: Workspace;
  busy: boolean;
  notice: { status: number; detail?: string } | null;
  composerEnabled: boolean;
  onVerify: () => void;
  onApproveHost: (host: string) => void;
  onBackToConfigure: () => void;
  onContinueFinalize: () => void;
}) {
  const phase = verifyPhase(ws.status, ws.verify_result);
  const running = ws.status === "building" || ws.status === "verifying";
  const result = ws.verify_result;

  // Live checklist: merge approved setup commands with the (possibly streamed)
  // verify_result so pending steps show up front and the running step is flagged.
  const commands = ws.setup_commands ?? [];
  const rows = verifyRows(commands, result);
  const { started, total } = verifyProgress(commands, result);
  const hasSteps = (result?.steps?.length ?? 0) > 0;
  const showChecklist = running || hasSteps;

  // Least-privilege one-click fix: denied hosts from run history the operator can
  // approve, then re-verify. Fetched on demand.
  const [observed, setObserved] = React.useState<{ denied: string[]; runs_examined: number } | null>(null);
  const [observedLoading, setObservedLoading] = React.useState(false);
  const checkDenied = async () => {
    setObservedLoading(true);
    try {
      setObserved(await workspacesApi.getObservedEgress(ws.id));
    } catch (e) {
      toast.error("Failed to load denied egress", { description: msg(e) });
    } finally {
      setObservedLoading(false);
    }
  };
  const deniedNotApproved = (observed?.denied ?? []).filter((h) => !(ws.approved_egress ?? []).includes(h));

  // AGENTIC fix (the deterministic egress button's sibling): ask a composer backend
  // to diagnose the failure. Human-gated (explicit click), ADVISORY (prose the
  // operator reads and applies via the affordances above/in Configure) — never
  // auto-applied. Self-contained here, mirroring checkDenied's on-demand fetch.
  const [aiSuggestion, setAiSuggestion] = React.useState<string | null>(null);
  const [aiLoading, setAiLoading] = React.useState(false);
  const [aiError, setAiError] = React.useState<string | null>(null);
  const askAi = async () => {
    setAiLoading(true);
    setAiError(null);
    try {
      setAiSuggestion(await workspacesApi.suggestVerifyFix(ws.id));
    } catch (e) {
      setAiError(msg(e));
    } finally {
      setAiLoading(false);
    }
  };

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2">
        <SectionLabel>Environment verification</SectionLabel>
        <Chip tone={VERIFY_PHASE_TONE[phase]} dot pulse={running}>
          {VERIFY_PHASE_LABEL[phase]}
        </Chip>
      </div>

      {/* 503: honest no-runner path — can't verify, but finalize is still allowed. */}
      {notice?.status === 503 && (
        <div
          className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5 text-xs text-warning"
          data-testid="verify-no-runner"
        >
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <div className="space-y-2">
            <p>
              Verification needs a runner (this control plane runs{" "}
              <span className="font-mono">-runner none</span>); you can still finalize as configured.
            </p>
            <Button size="sm" variant="outline" onClick={onContinueFinalize}>
              Finalize as configured
            </Button>
          </div>
        </div>
      )}

      {/* 422: no approved commands — send them back to Configure. */}
      {notice?.status === 422 && (
        <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5 text-xs text-warning">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <div className="space-y-2">
            <p>{notice.detail || "Approve at least one setup command before verifying."}</p>
            <Button size="sm" variant="outline" onClick={onBackToConfigure}>
              Back to Configure
            </Button>
          </div>
        </div>
      )}

      {/* 409: a verify is already running. */}
      {notice?.status === 409 && (
        <p className="text-xs text-muted-foreground">
          {notice.detail || "A verification is already running for this workspace."}
        </p>
      )}

      {/* In-flight progress header — "step N of total" + a slim bar. */}
      {running && (
        <div className="space-y-1.5" data-testid="verify-progress">
          <p className="text-sm text-muted-foreground">
            {total > 0
              ? `Verifying environment — step ${started} of ${total}`
              : "Verifying environment…"}
          </p>
          {total > 0 && (
            <div className="h-1.5 overflow-hidden rounded-full bg-surface-2">
              <div
                className="h-full rounded-full bg-primary transition-all"
                style={{ width: `${Math.min(100, Math.round((started / total) * 100))}%` }}
              />
            </div>
          )}
        </div>
      )}

      {/* Per-command checklist: pending (waiting) → running (streamed log) → done
          pass (collapsed logs) / fail (expanded logs). Derived from the approved
          setup commands so not-yet-started steps are visible up front. */}
      {showChecklist && rows.length > 0 && (
        <div className="space-y-2" data-testid="verify-steps">
          {rows.map((r, i) => (
            <VerifyStepRow key={i} row={r} />
          ))}
        </div>
      )}

      {/* Honest environmental-failure hint (toolchain missing / Maven proxy /
          GOTMPDIR) — server-classified, shown above the egress-fix so a 127 or
          "permission denied" isn't misread as a denied-host problem. */}
      {!running && result && !result.ok && result.failure_hint && (
        <div
          className="rounded-lg border border-amber-500/40 bg-amber-500/5 p-3 text-[0.6875rem] leading-snug text-foreground"
          data-testid="verify-failure-hint"
        >
          {result.failure_hint}
        </div>
      )}

      {/* One-click fix from denied egress — only on a settled failure/partial. */}
      {!running && result && !result.ok && (
        <div className="space-y-2 rounded-lg border border-border p-3" data-testid="verify-fix">
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            A build often fails because a needed host was denied. Approve a denied host, then re-run.
          </p>
          {observed === null ? (
            <Button size="sm" variant="outline" onClick={checkDenied} disabled={observedLoading}>
              {observedLoading ? <Loader2 className="size-3.5 animate-spin" /> : <ScanSearch className="size-3.5" />}
              Suggest a fix from denied egress
            </Button>
          ) : deniedNotApproved.length === 0 ? (
            <p className="text-[0.6875rem] text-muted-foreground">
              No denied egress to approve from {observed.runs_examined} recent run
              {observed.runs_examined === 1 ? "" : "s"}.
            </p>
          ) : (
            <ul className="space-y-1.5" data-testid="verify-fix-hosts">
              {deniedNotApproved.map((h) => (
                <li key={h} className="flex items-center gap-2">
                  <Mono className="flex-1 text-foreground">{h}</Mono>
                  <Button size="sm" variant="outline" className="h-7" onClick={() => onApproveHost(h)}>
                    <Check className="size-3.5" /> Approve
                  </Button>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}

      {/* AGENTIC fix — a composer backend diagnoses the failure. Hidden entirely
          when no composer is configured (suggest-fix would 404). Advisory + human-
          gated: the operator clicks to ask, reads the suggestion, and applies it
          via the Approve buttons above or in Configure — nothing is auto-applied. */}
      {!running && result && !result.ok && composerEnabled && (
        <div className="space-y-2 rounded-lg border border-border p-3" data-testid="verify-ai-fix">
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            Or ask AI to diagnose the failure from the (secret-masked) logs. It suggests one likely
            fix — a host to allow, a secret to add, or a corrected command — for you to apply. It
            never changes anything on its own.
          </p>
          {aiSuggestion === null ? (
            <Button size="sm" variant="outline" onClick={askAi} disabled={aiLoading}>
              {aiLoading ? <Loader2 className="size-3.5 animate-spin" /> : <Sparkles className="size-3.5" />}
              Diagnose with AI
            </Button>
          ) : (
            <div className="space-y-2" data-testid="verify-ai-suggestion">
              <CopyableBlock text={aiSuggestion} />
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">
                Suggested by the model — you decide. Approve any host above, or add a secret / edit a
                command back in Configure.
              </p>
            </div>
          )}
          {aiError && (
            <p className="flex items-start gap-1.5 text-[0.6875rem] text-danger" data-testid="verify-ai-error">
              <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
              {aiError}
            </p>
          )}
        </div>
      )}

      {phase === "success" && result?.ran && (
        <p className="flex items-center gap-1.5 text-sm text-success">
          <CircleCheck className="size-4" /> The environment built and verified successfully.
        </p>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <Button onClick={onVerify} disabled={busy || running}>
          {busy || running ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4" />}
          {result ? "Re-run verify" : "Verify environment"}
        </Button>
      </div>
    </div>
  );
}

// One row of the verify checklist. Pending shows "waiting"; running streams its
// log tail live under a gerund label; a passed step collapses its logs behind a
// "show output" toggle; a failed step shows exit/timeout + expanded logs (the
// detail the operator needs). All log strings are already server-masked — rendered
// VERBATIM, never unmasked.
export function VerifyStepRow({ row }: { row: VerifyRow }) {
  const [open, setOpen] = React.useState(false);
  const step = row.step;
  const dur = fmtStepDuration(step?.duration_ms);
  const logHead = step?.log_head;
  const logTail = step?.log_tail;
  const hasLog = !!(logHead || logTail);

  return (
    <div className="rounded-lg border border-border p-3">
      <div className="flex flex-wrap items-center gap-2">
        {row.state === "pending" && (
          <span className="size-2 shrink-0 rounded-full bg-muted-foreground/40" aria-hidden />
        )}
        {row.state === "running" && <Loader2 className="size-4 shrink-0 animate-spin text-primary" />}
        {row.state === "pass" && <CircleCheck className="size-4 shrink-0 text-success" />}
        {row.state === "fail" && <CircleX className="size-4 shrink-0 text-danger" />}

        <Chip tone="neutral">{row.stage}</Chip>
        {row.state === "running" ? (
          <span className="text-sm text-foreground">{runningLabel(row)}</span>
        ) : (
          <Mono className="text-foreground">{row.command}</Mono>
        )}

        <span className="ml-auto flex items-center gap-2">
          {row.state === "pending" && <span className="text-xs text-muted-foreground">waiting</span>}
          {row.state === "pass" && (
            <>
              {dur && <span className="text-xs text-muted-foreground">{dur}</span>}
              {hasLog && (
                <button
                  onClick={() => setOpen((o) => !o)}
                  className="text-xs text-muted-foreground underline-offset-2 hover:underline"
                >
                  {open ? "hide output" : "show output"}
                </button>
              )}
            </>
          )}
          {row.state === "fail" && (
            <Chip tone="danger" dot>
              {step?.timed_out ? "timed out" : `exit ${step?.exit_code}`}
            </Chip>
          )}
        </span>
      </div>

      {/* running: stream the tail (fall back to head) so the operator sees progress */}
      {row.state === "running" && (logTail || logHead) && <LogPre text={logTail || logHead || ""} />}
      {/* pass: logs collapsed behind the toggle */}
      {row.state === "pass" && open && (
        <>
          {logHead && <LogPre text={logHead} />}
          {logTail && <LogPre text={logTail} />}
        </>
      )}
      {/* fail: expand head + tail — this is the failure to read */}
      {row.state === "fail" && (
        <>
          {logHead && <LogPre text={logHead} />}
          {logTail && <LogPre text={logTail} />}
        </>
      )}
    </div>
  );
}

// A small scrollable log excerpt. Text is already masked server-side; rendered
// as-is inside a fixed-height scroller so a long tail doesn't blow out the row.
export function LogPre({ text }: { text: string }) {
  return (
    <pre className="scroll-thin mt-2 max-h-40 overflow-auto rounded-md bg-surface-2/60 p-2 text-[0.6875rem] leading-relaxed text-muted-foreground">
      <code className="font-mono">{text}</code>
    </pre>
  );
}

// ------------------------------------------------------------
// Finalize
// ------------------------------------------------------------
function FinalizePane({
  ws,
  emitCode,
  onEmitCodeChange,
  finalizing,
  emitted,
  onFinalize,
  onDone,
}: {
  ws: Workspace;
  emitCode: boolean;
  onEmitCodeChange: (v: boolean) => void;
  finalizing: boolean;
  emitted: Record<string, string> | null;
  onFinalize: () => void;
  onDone: () => void;
}) {
  if (emitted) {
    const files = Object.entries(emitted);
    return (
      <div className="space-y-4">
        <p className="flex items-center gap-1.5 text-sm text-success">
          <CircleCheck className="size-4" /> {ws.name} imported.
        </p>
        {files.length > 0 && (
          <section className="space-y-3">
            <SectionLabel>Emitted files — commit these to keep the environment reproducible</SectionLabel>
            {files.map(([name, content]) => (
              <div key={name} className="space-y-1.5">
                <div className="flex items-center gap-1.5 text-xs font-medium text-foreground">
                  <FileCode2 className="size-3.5 text-cyan" /> {name}
                </div>
                <CopyableBlock text={content} />
              </div>
            ))}
          </section>
        )}
        <Button onClick={onDone}>Done</Button>
      </div>
    );
  }

  return (
    <div className="space-y-4">
      <SecurityChip />
      <p className="text-sm text-muted-foreground">
        Lock in the reviewed profile as this workspace&apos;s environment. Runs will reuse it.
      </p>
      <label className="flex cursor-pointer items-start gap-2.5 rounded-lg border border-border p-3">
        <Checkbox
          checked={emitCode}
          onCheckedChange={(v) => onEmitCodeChange(!!v)}
          className="mt-0.5"
          aria-label="Emit devcontainer.json and AGENTS.md"
        />
        <span className="space-y-0.5">
          <span className="block text-sm font-medium text-foreground">
            Emit devcontainer.json / AGENTS.md
          </span>
          <span className="block text-[0.6875rem] leading-snug text-muted-foreground">
            Generate committable environment-as-code files from the reviewed profile.
          </span>
        </span>
      </label>
      <Button onClick={onFinalize} disabled={finalizing}>
        {finalizing ? <Loader2 className="size-4 animate-spin" /> : <CircleCheck className="size-4" />}
        Finalize import
      </Button>
    </div>
  );
}

// A minimal copy-to-clipboard code block for arbitrary emitted-file TEXT (JsonBlock
// pretty-prints a JSON value; these are raw file contents). Same affordance shape.
function CopyableBlock({ text }: { text: string }) {
  const { copied, copy } = useCopyToClipboard();
  return (
    <div className="group relative rounded-lg border border-border bg-surface-2/60">
      <button
        onClick={() => copy(text)}
        className="absolute right-2 top-2 z-10 inline-flex size-7 items-center justify-center rounded-md border border-border bg-card text-muted-foreground opacity-0 transition group-hover:opacity-100 hover:text-foreground"
        aria-label="Copy"
      >
        {copied ? <Check className="size-3.5 text-success" /> : <Copy className="size-3.5" />}
      </button>
      <pre className="scroll-thin overflow-x-auto p-3 text-xs leading-relaxed">
        <code className="font-mono">{text}</code>
      </pre>
    </div>
  );
}
