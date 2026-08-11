/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// WORKSPACE DETAIL — the addressable hub at /workspaces/:id. Same breadcrumb/
// header/section-card idiom as run-detail.tsx. Replaces the retired guided
// Import dialog: everything past onboarding (requirements, detected-but-not-
// required candidates, sessions, env-as-code) now lives on this page instead
// of a step rail, so it survives navigation and never has to be "resumed".
import * as React from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { AlertTriangle, ChevronRight, Loader2, MoreHorizontal } from "lucide-react";
import { toast } from "sonner";
import type { Workspace } from "../../../lib/types";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { setup as setupApi } from "../../../lib/api/setup";
import { runs as runsApi } from "../../../lib/api/runs";
import { getErrorMessage } from "../../../lib/format";
import { usePoll } from "../../../lib/use-poll";
import { statusTone, statusWord, storySentence } from "../../../lib/workspace-status";
import { hasLlmPath } from "../onboarding/intro";
import { comesWithLine } from "../new-run/wizard-types";
import { Button } from "../../ui/button";
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
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "../../ui/dropdown-menu";
import { Mono } from "../../wardyn/code-block";
import { CopyButton } from "../../wardyn/copy-button";
import { ConfirmEgressDialog } from "../../wardyn/confirm-egress-dialog";
import { Chip, OperatorOnlyHint } from "../../wardyn/primitives";
import { DeleteConfirmDialog } from "../../wardyn/delete-confirm-dialog";
import { EmptyState, ErrorState, TableSkeleton } from "../../wardyn/states";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";
import { useOperator } from "../../wardyn/operator-context";
import { KIND_META, attentionItems, sourceSubLine } from "../workspaces";
import { WorkspaceWizard } from "../workspace-wizard/wizard";
import { ProfileReview } from "../profile-review";
import { SectionCard } from "./section-card";
import { RequirementsCard } from "./requirements-card";
import { DetectedCard } from "./detected-card";
import { EnvAsCodeCard } from "./env-as-code-card";
import { RecordPane } from "./record-pane";
import { isRecording, newEgressHosts, sessionKeyOf } from "./session-helpers";

const POLL_MS = 2500;

export function WorkspaceDetailScreen() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const operator = useOperator();

  const [ws, setWs] = React.useState<Workspace | null | undefined>(undefined);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [secretNames, setSecretNames] = React.useState<string[]>([]);
  const [llmReady, setLlmReady] = React.useState(false);

  const [editOpen, setEditOpen] = React.useState(false);
  const [rescanOpen, setRescanOpen] = React.useState(false);
  const [confirmDelete, setConfirmDelete] = React.useState(false);

  const load = React.useCallback(
    (foreground: boolean) => {
      if (!id) return;
      if (foreground) setStatus("loading");
      workspacesApi
        .getWorkspace(id)
        .then((w) => {
          setWs(w ?? null);
          setStatus("ready");
        })
        .catch(() => {
          if (foreground) setStatus("error");
        });
    },
    [id],
  );

  React.useEffect(() => {
    setWs(undefined);
    setStatus("loading");
    load(true);
  }, [id, load]);

  const loadSecrets = React.useCallback(() => {
    secretsApi.listSecrets().then(setSecretNames).catch(() => {});
  }, []);
  React.useEffect(() => {
    loadSecrets();
    setupApi
      .getSetupStatus()
      .then((s) => setLlmReady(hasLlmPath(s)))
      .catch(() => setLlmReady(false));
  }, [loadSecrets]);

  // Poll while a scan or any session (open record / confined replay) is
  // in-flight server-side, so the page reflects it without a manual refresh —
  // paused otherwise so a settled workspace isn't polled forever.
  const inFlight = !!ws && (ws.status === "scanning" || isRecording(ws));
  usePoll(() => load(false), POLL_MS, !(id && inFlight));

  // ---------------- scan ----------------
  const scan = async () => {
    if (!ws) return;
    try {
      const { async: isAsync } = await workspacesApi.scanWorkspace(ws.id);
      if (isAsync) toast.info(`Scan started for "${ws.name}"`, { description: "Watch it under Runs, or wait here." });
    } catch (e) {
      toast.error(`Failed to scan "${ws.name}"`, { description: getErrorMessage(e) });
    } finally {
      load(false);
    }
  };

  // ---------------- sessions (record / confined replay) ----------------
  const [recordBusyTask, setRecordBusyTask] = React.useState<string | null>(null);
  const [recordNotice, setRecordNotice] = React.useState<{ status: number; detail?: string } | null>(null);
  const [profileRunId, setProfileRunId] = React.useState<string | null>(null);
  const [profileName, setProfileName] = React.useState<string | undefined>(undefined);
  const [pendingConfirm, setPendingConfirm] = React.useState<{ hosts: string[]; run: () => void } | null>(null);

  const doRecord = async (name: string, confined: boolean) => {
    if (!ws) return;
    setRecordBusyTask((confined ? "verify:" : "") + sessionKeyOf(name));
    setRecordNotice(null);
    try {
      const r = await workspacesApi.recordTask(ws.id, name, confined);
      if (!r.ok) setRecordNotice({ status: r.status, detail: r.detail });
      load(false);
    } catch (e) {
      toast.error(confined ? "Confined replay failed to start" : "Recording failed to start", {
        description: getErrorMessage(e),
      });
    } finally {
      setRecordBusyTask(null);
    }
  };

  // "Done" on either an open recording or a confined replay — the backend
  // captures on termination either way; the poll above picks up the result.
  // No unmount-time kill anywhere in this screen: a session survives
  // navigation (C.SESSION_SURVIVES) — stop it explicitly from here or Runs.
  const doneRecording = async (runId: string) => {
    try {
      await runsApi.killRun(runId);
      load(false);
    } catch (e) {
      toast.error("Failed to stop the session", { description: getErrorMessage(e) });
    }
  };

  const promoteEgress = async (taskKey: string) => {
    if (!ws) return;
    const rr = ws.record_results?.[taskKey];
    const observedAllowed = (rr?.observations?.domains ?? []).filter((d) => d.allow_count > 0).map((d) => d.host);
    const fallback = Array.from(new Set([...(ws.approved_egress ?? []), ...observedAllowed]));
    try {
      setWs(await workspacesApi.promoteRecordEgress(ws.id, taskKey, fallback));
      toast.success("Approved observed egress");
    } catch (e) {
      toast.error("Failed to promote egress", { description: getErrorMessage(e) });
    }
  };
  const approveHost = async (host: string) => {
    if (!ws) return;
    try {
      setWs(await workspacesApi.setApprovedEgress(ws.id, [...(ws.approved_egress ?? []), host]));
      toast.success(`Approved egress to ${host}`);
    } catch (e) {
      toast.error("Failed to approve host", { description: getErrorMessage(e) });
    }
  };
  // Both routed through the SAME untrusted-content confirm the rest of the
  // app uses for egress approval — the host names came from a session's
  // observed traffic, not something the operator typed.
  const requestApproveHost = (host: string) => setPendingConfirm({ hosts: [host], run: () => void approveHost(host) });
  // UI-WS-14: newEgressHosts is the SAME helper that drives the button's own
  // "Approve N observed host(s)" count (record-pane.tsx) — reusing it here
  // means the untrusted-content confirm can never list more hosts than the
  // button that opened it offered (it used to subtract only approved_egress,
  // missing the profile's own auto-allowed set).
  const requestPromoteEgress = (taskKey: string) => {
    if (!ws) return;
    setPendingConfirm({ hosts: newEgressHosts(ws, taskKey), run: () => void promoteEgress(taskKey) });
  };

  // ---------------- render ----------------
  if (status === "loading") {
    return (
      <div className="mx-auto max-w-[1000px] px-6 py-5">
        <div className="space-y-4">
          <div className="rounded-xl border border-border bg-card p-5">
            <div className="h-6 w-72 animate-pulse rounded bg-muted" />
            <div className="mt-3 h-4 w-96 animate-pulse rounded bg-muted" />
          </div>
          <div className="rounded-xl border border-border bg-card">
            <TableSkeleton rows={4} cols={2} />
          </div>
        </div>
      </div>
    );
  }
  if (status === "error") {
    return (
      <div className="mx-auto max-w-[1000px] px-6 py-5">
        <div className="rounded-xl border border-border bg-card">
          <ErrorState onRetry={() => load(true)} />
        </div>
      </div>
    );
  }
  if (!ws) {
    return (
      <div className="mx-auto max-w-[1000px] px-6 py-5">
        <div className="rounded-xl border border-border bg-card">
          <EmptyState
            icon={AlertTriangle}
            title="Workspace not found"
            description="It may have been deleted, or the link is stale."
            action={
              <Button variant="outline" onClick={() => navigate("/workspaces")}>
                Back to Workspaces
              </Button>
            }
          />
        </div>
      </div>
    );
  }

  const kindMeta = KIND_META[ws.kind] ?? KIND_META.local_dir;
  const tone = statusTone(ws.status);
  const attention = attentionItems(ws, secretNames);
  const comesWith = comesWithLine(ws);

  let primary: React.ReactNode;
  if (ws.status === "scanning") {
    primary = (
      <span className="inline-flex items-center gap-2">
        <Loader2 className="size-3.5 animate-spin text-muted-foreground" />
        <Button
          size="sm"
          variant="outline"
          disabled={!ws.active_run_id}
          onClick={() => ws.active_run_id && navigate(`/runs/${encodeURIComponent(ws.active_run_id)}`)}
        >
          Watch the run →
        </Button>
      </span>
    );
  } else if (ws.status === "error") {
    primary = (
      <Button size="sm" disabled={!operator} title={operator ? undefined : OPERATOR_ONLY_REASON} onClick={() => void scan()}>
        Retry scan
      </Button>
    );
  } else if (ws.status === "pending_scan") {
    primary = (
      <Button size="sm" disabled={!operator} title={operator ? undefined : OPERATOR_ONLY_REASON} onClick={() => void scan()}>
        Scan now
      </Button>
    );
  } else {
    // Runs' NewRunDialog opens on a workspace-first picker where this
    // workspace is already one of the cards — no pre-seed deep-link needed
    // (#10/D14, pass3-ux-proposal.md §4). Route state just tells /runs to
    // open the dialog on arrival.
    primary = (
      <Button size="sm" onClick={() => navigate("/runs", { state: { openNewRun: true } })}>
        Start a run
      </Button>
    );
  }

  return (
    <div className="mx-auto max-w-[1000px] px-6 py-5">
      <div className="mb-4 flex items-center gap-2 text-sm">
        <Link to="/workspaces" className="text-muted-foreground hover:text-foreground hover:underline">
          Workspaces
        </Link>
        <ChevronRight className="size-3.5 text-muted-foreground" aria-hidden />
        <span className="text-foreground">{ws.name}</span>
      </div>

      <div className="rounded-xl border border-border bg-card p-5">
        <div className="flex flex-wrap items-start gap-3">
          <span className="inline-flex size-9 shrink-0 items-center justify-center rounded-lg border border-border text-muted-foreground">
            <kindMeta.Icon className="size-4" />
          </span>
          <div className="min-w-0 flex-1">
            <div className="flex flex-wrap items-center gap-2">
              <h1 className="text-lg font-semibold text-foreground">{ws.name}</h1>
              <Chip tone={tone.tone} dot pulse={tone.pulse}>
                {statusWord(ws.status)}
              </Chip>
            </div>
            <p className="mt-1 text-xs text-muted-foreground">{storySentence(ws)}</p>
            <div className="mt-2 flex flex-wrap items-center gap-1.5">
              <Mono className="text-xs text-foreground" title={sourceSubLine(ws)}>
                {sourceSubLine(ws)}
              </Mono>
              <CopyButton text={ws.source} iconClassName="size-3" className="size-6 justify-center rounded-md border border-border text-muted-foreground hover:text-foreground" />
            </div>
            <p className="mt-2 text-xs text-muted-foreground">
              <strong className="text-foreground">Comes with:</strong> {comesWith}
            </p>
            {attention.length > 0 && (
              <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1">
                {attention.map((a, i) => (
                  <span
                    key={i}
                    className={
                      "text-[0.6875rem] " +
                      (a.tone === "danger" ? "text-danger" : a.tone === "warning" ? "text-warning" : "text-muted-foreground")
                    }
                  >
                    {a.text}
                  </span>
                ))}
              </div>
            )}
          </div>
          {primary}
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button size="sm" variant="ghost" className="size-8 p-0" aria-label="Workspace actions">
                <MoreHorizontal className="size-4" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem onClick={() => setEditOpen(true)} disabled={!operator}>
                Edit workspace…
                {!operator && <OperatorOnlyHint />}
              </DropdownMenuItem>
              <DropdownMenuItem onClick={() => setRescanOpen(true)} disabled={!operator}>
                Rescan…
                {!operator && <OperatorOnlyHint />}
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                className="text-danger focus:text-danger"
                disabled={!operator}
                onClick={() => setConfirmDelete(true)}
              >
                Delete…
                {!operator && <OperatorOnlyHint />}
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      <div className="mt-4 flex flex-col gap-4">
        <RequirementsCard ws={ws} storedSecretNames={secretNames} onWorkspaceUpdated={setWs} onSecretStored={(n) => setSecretNames((s) => [...s, n])} />

        <DetectedCard ws={ws} onWorkspaceUpdated={setWs} />

        <SectionCard title="Sessions" subtitle="Learn what it really uses: record a live session in an open sandbox, promote what it reached, replay it confined.">
          <RecordPane
            ws={ws}
            notice={recordNotice}
            busyTask={recordBusyTask}
            modelReady={llmReady}
            onRecord={(name) => void doRecord(name, false)}
            onReplayConfined={(name) => void doRecord(name, true)}
            onDoneRecording={(runId) => void doneRecording(runId)}
            onPromoteEgress={requestPromoteEgress}
            onApproveHost={requestApproveHost}
            onOpenProfile={(runId, suggested) => {
              setProfileRunId(runId);
              setProfileName(suggested);
            }}
          />
        </SectionCard>

        <EnvAsCodeCard ws={ws} />
      </div>

      {editOpen && (
        <WorkspaceWizard
          key={ws.id}
          origin="library"
          initial={ws}
          onClose={() => {
            setEditOpen(false);
            load(false);
          }}
        />
      )}

      <AlertDialog open={rescanOpen} onOpenChange={setRescanOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Rescan {ws.name}?</AlertDialogTitle>
            {/* UX-6: this action is POST /workspaces/{id}/scan — a plain
                re-read, not the composition-edit PUT that actually clears
                requirements/sessions (that warning belongs on wizard.tsx's
                own confirm, where it's true). Reusing C.RESCAN_DESTROYS here
                deterred the one safe way to refresh a stale profile. */}
            <AlertDialogDescription>
              Rescanning re-reads each source from scratch and refreshes what it detected — your requirements,
              approved egress, and recorded sessions aren&apos;t touched.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                setRescanOpen(false);
                void scan();
              }}
            >
              Rescan
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <DeleteConfirmDialog
        name={confirmDelete ? ws.name : null}
        entity="workspace"
        description="Existing runs keep their history and audit trail. New runs can no longer attach it; its requirements and recorded sessions are removed."
        onOpenChange={(o) => !o && setConfirmDelete(false)}
        onDelete={() => workspacesApi.deleteWorkspace(ws.id)}
        onDeleted={() => navigate("/workspaces")}
      />

      <ProfileReview runId={profileRunId} suggestedName={profileName} onClose={() => setProfileRunId(null)} />

      <ConfirmEgressDialog
        hosts={pendingConfirm?.hosts?.length ? pendingConfirm.hosts : null}
        onOpenChange={(o) => !o && setPendingConfirm(null)}
        onConfirm={() => {
          const run = pendingConfirm?.run;
          setPendingConfirm(null);
          run?.();
        }}
      />
    </div>
  );
}
