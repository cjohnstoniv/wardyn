/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// WORKSPACE DETAIL — the addressable hub at /workspaces/:id. Three cards:
// Recorded sessions (RecordPane — Record Mode, untouched), Allowed hosts, and
// Denied hosts (Phase 4 revocation — the only console surface for undoing a
// `deny · always` decision). A workspace is usable the instant it's created
// (POST /workspaces already accepts {name, sources[], base_image?}), so this
// page no longer surfaces scan/build machinery at all — Stage 2 stops calling
// those endpoints.
import * as React from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { AlertTriangle, ChevronRight, Trash2 } from "lucide-react";
import { toast } from "sonner";
import type { ConfinementClass, Workspace } from "../../../lib/types";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import { setup as setupApi } from "../../../lib/api/setup";
import { runs as runsApi } from "../../../lib/api/runs";
import { getErrorMessage } from "../../../lib/format";
import { usePoll } from "../../../lib/use-poll";
import { hasLlmPath } from "../../../lib/readiness";
import { Button } from "../../ui/button";
import { CopyButton } from "../../wardyn/copy-button";
import { ConfirmEgressDialog } from "../../wardyn/confirm-egress-dialog";
import { OperatorOnlyHint } from "../../wardyn/primitives";
import { DeleteConfirmDialog } from "../../wardyn/delete-confirm-dialog";
import { EmptyState, ErrorState, TableSkeleton } from "../../wardyn/states";
import { useOperator } from "../../wardyn/operator-context";
import { KIND_META, workspaceImage } from "../workspaces";
import { ProfileReview } from "../profile-review";
import { DetailSectionCard } from "./section-card";
import { AllowedHostsCard } from "./allowed-hosts-card";
import { DeniedHostsCard } from "./denied-hosts-card";
import { RecordPane } from "./record-pane";
import { isRecording, newEgressHosts, sessionKeyOf } from "./session-helpers";

const POLL_MS = 2500;

// The header's muted mono line — kind · source[ · ref], e.g. "repo ·
// github.com/acme/api · main". Distinct from sourceSubLine (the list's own
// Source column): this line always names the kind up front, matching the
// approved mock verbatim for a repo (`repo · github.com/acme/api · main`).
function detailSourceLine(ws: Workspace): string {
  if (!ws.source) return "empty — discarded after the run";
  const kindLabel = KIND_META[ws.kind]?.label ?? ws.kind;
  return ws.kind === "repo" && ws.ref ? `${kindLabel} · ${ws.source} · ${ws.ref}` : `${kindLabel} · ${ws.source}`;
}

// The image row's mono value + blurb, per CANON-STRINGS.md's three variants.
// Only the devcontainer copy is captured verbatim there ("Built as written —
// we don't modify it."); the other two are written to match its tone since
// the mock didn't capture them.
function imageRow(ws: Workspace): { mono: string; blurb: string; rebuildable: boolean } {
  const image = workspaceImage(ws);
  if (image.kind === "devcontainer") {
    const repoSuffix = ws.kind === "repo" ? ` (this repo${ws.ref ? `, @${ws.ref}` : ""})` : "";
    return { mono: `.devcontainer/devcontainer.json${repoSuffix}`, blurb: "Built as written — we don't modify it.", rebuildable: true };
  }
  if (image.kind === "ref") {
    return { mono: image.label, blurb: "Pinned — Wardyn pulls this image exactly as given.", rebuildable: false };
  }
  return { mono: "standard sandbox image", blurb: "Wardyn's baseline container — no project-specific build.", rebuildable: false };
}

export function WorkspaceDetailScreen() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const operator = useOperator();

  const [ws, setWs] = React.useState<Workspace | null | undefined>(undefined);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [llmReady, setLlmReady] = React.useState(false);
  // The runner's declared confinement classes — RecordPane keys its open-egress
  // banner's tier line off the STRONGEST of these (what a recording actually
  // launches under, workspace_run.go's bestClass) rather than the operator's
  // New-Run default, which once printed "Fence" under a Vault capture.
  const [hostClasses, setHostClasses] = React.useState<ConfinementClass[] | null>(null);
  const [rebuilding, setRebuilding] = React.useState(false);
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

  React.useEffect(() => {
    setupApi
      .getSetupStatus()
      .then((s) => {
        setLlmReady(hasLlmPath(s));
        setHostClasses(s.runner?.confinement_classes ?? null);
      })
      .catch(() => setLlmReady(false));
  }, []);

  // Poll while a session (open record / confined replay) is in-flight
  // server-side, so the page reflects it without a manual refresh — paused
  // otherwise so a settled workspace isn't polled forever.
  const inFlight = !!ws && isRecording(ws);
  usePoll(() => load(false), POLL_MS, !(id && inFlight));

  const rebuild = async () => {
    if (!ws) return;
    setRebuilding(true);
    try {
      await workspacesApi.buildWorkspace(ws.id);
      toast.success(`Rebuild started for "${ws.name}"`);
      load(false);
    } catch (e) {
      toast.error("Failed to start the rebuild", { description: getErrorMessage(e) });
    } finally {
      setRebuilding(false);
    }
  };

  // ---------------- sessions (record / confined replay) ----------------
  const [recordBusyTask, setRecordBusyTask] = React.useState<string | null>(null);
  const [recordNotice, setRecordNotice] = React.useState<{ status: number; detail?: string } | null>(null);
  // The launch's own warnings (open-egress exfiltration window on weak
  // confinement, the masking caveat) + its REAL confinement class — the
  // server's own facts about THIS launch, never dropped and never guessed
  // from the operator's persisted default tier.
  const [recordLaunch, setRecordLaunch] = React.useState<{ warnings?: string[]; confinementClass?: string } | null>(null);
  const [profileRunId, setProfileRunId] = React.useState<string | null>(null);
  const [profileName, setProfileName] = React.useState<string | undefined>(undefined);
  const [pendingConfirm, setPendingConfirm] = React.useState<{
    hosts: string[];
    selectable?: boolean;
    // Receives what the dialog actually approved (the checked subset when
    // selectable), never the full list it was opened with.
    run: (approved: string[]) => void;
  } | null>(null);

  const doRecord = async (name: string, confined: boolean) => {
    if (!ws) return;
    setRecordBusyTask((confined ? "verify:" : "") + sessionKeyOf(name));
    setRecordNotice(null);
    setRecordLaunch(null);
    try {
      const r = await workspacesApi.recordTask(ws.id, name, confined);
      if (!r.ok) {
        setRecordNotice({ status: r.status, detail: r.detail });
      } else {
        setRecordLaunch({ warnings: r.warnings, confinementClass: r.confinement_class });
      }
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
  // navigation — stop it explicitly from here or Runs.
  const doneRecording = async (runId: string) => {
    try {
      await runsApi.killRun(runId);
      load(false);
    } catch (e) {
      toast.error("Failed to stop the session", { description: getErrorMessage(e) });
    }
  };

  // `hosts` is the subset the confirm dialog's checkboxes actually approved —
  // the server validates every one against the recording's promotable set
  // (record.go's handlePromoteRecordEgress) and rejects anything else. No 404
  // fallback: the console ships baked into the server image, so there is no
  // version-skew window, and the retired fallback re-approved the FULL
  // observed union — every host the operator just unchecked included.
  const promoteEgress = async (taskKey: string, hosts: string[]) => {
    if (!ws) return;
    try {
      setWs(await workspacesApi.promoteRecordEgress(ws.id, taskKey, hosts));
      toast.success("Approved observed egress");
    } catch (e) {
      toast.error("Failed to promote egress", { description: getErrorMessage(e) });
    }
  };
  // ONE write for N hosts, onto the REQUIREMENTS lane — the same lane promote
  // itself writes (egress:<host> · required · operator_set), retiring the last
  // ADDITIVE ApprovedEgress writer. Read-modify-write of the workspace's OWN
  // overlay (the PUT is a full replacement); last-write-wins is accepted, same
  // as the lane it replaces.
  //
  // TIER, since the move changed it: PUT /workspaces/{id}/requirements is
  // registered on operatorOnly, while the pane that offers these controls is
  // gated on the securityOps tier. The Approve controls therefore carry their
  // OWN useOperator gate, in CaughtHosts where they live (F031) — anything
  // that grows a new caller of approveHosts owes the same gate.
  const approveHosts = async (hosts: string[]) => {
    if (!ws || hosts.length === 0) return;
    try {
      // Merge onto the FRESHEST overlay, not the one captured at click time —
      // the confirm dialog can sit open for minutes, and the PUT is a full
      // replacement. Inside the try: a rejected fetch here used to reject the
      // whole call, which the guided approve→replay chain void-discarded — the
      // replay silently never fired, with no toast to say why.
      const fresh = (await workspacesApi.getWorkspace(ws.id).catch(() => null)) ?? ws;
      const next = { ...(fresh.requirements ?? {}) };
      for (const host of hosts) next[`egress:${host}`] = { level: "required", provenance: "operator_set" };
      setWs(await workspacesApi.setRequirements(ws.id, next));
      toast.success(hosts.length === 1 ? `Approved egress to ${hosts[0]}` : `Approved egress to ${hosts.length} hosts`);
      return true;
    } catch (e) {
      toast.error("Failed to approve host", { description: getErrorMessage(e) });
      return false;
    }
  };
  // Both routed through the SAME untrusted-content confirm the rest of the
  // app uses for egress approval — the host names came from a session's
  // observed traffic, not something the operator typed. `replayName` is the
  // guided loop: once the approval has LANDED, replay that session confined
  // again, so the fresh verdict is measured against the widened set.
  const requestApproveHosts = (hosts: string[], replayName?: string) =>
    setPendingConfirm({
      hosts,
      run: (approved) =>
        void approveHosts(approved).then((ok) => {
          if (ok && replayName) void doRecord(replayName, true);
        }),
    });
  const requestPromoteEgress = (taskKey: string) => {
    if (!ws) return;
    setPendingConfirm({
      hosts: newEgressHosts(ws, taskKey, window.location.hostname),
      // Bulk promote is the one flow where the operator picks a SUBSET of what
      // a recording observed — the server's own optional {"hosts":[…]} field.
      selectable: true,
      run: (approved) => void promoteEgress(taskKey, approved),
    });
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
  const image = imageRow(ws);

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
            <h1 className="text-lg font-semibold text-foreground">{ws.name}</h1>
            <div className="mt-1 flex flex-wrap items-center gap-1.5">
              <span className="font-mono text-xs text-muted-foreground">{detailSourceLine(ws)}</span>
              {ws.source && (
                <CopyButton
                  text={ws.source}
                  label="Copy source path"
                  iconClassName="size-3"
                  className="size-6 justify-center rounded-md border border-border text-muted-foreground hover:text-foreground"
                />
              )}
            </div>
          </div>
          <Button size="sm" onClick={() => navigate("/runs", { state: { openNewRun: true } })}>
            Start a run
          </Button>
          <Button
            size="sm"
            variant="ghost"
            className="size-8 p-0 text-danger hover:text-danger"
            disabled={!operator}
            title={operator ? "Delete this workspace" : undefined}
            aria-label="Delete this workspace"
            onClick={() => setConfirmDelete(true)}
          >
            <Trash2 className="size-4" />
            {!operator && <OperatorOnlyHint />}
          </Button>
        </div>

        <div className="mt-3 flex flex-wrap items-center gap-3 rounded-lg border border-border bg-surface-2/60 px-3 py-2.5">
          <div className="min-w-0 flex-1">
            <p className="text-xs text-foreground">
              Image <span className="font-mono">{image.mono}</span>
            </p>
            <p className="mt-0.5 text-meta text-muted-foreground">{image.blurb}</p>
          </div>
          {image.rebuildable && (
            <Button size="sm" variant="outline" disabled={!operator || rebuilding} onClick={() => void rebuild()}>
              Rebuild
            </Button>
          )}
        </div>
      </div>

      <div className="mt-4 flex flex-col gap-4">
        <DetailSectionCard
          title="Recorded sessions"
          subtitle='Run a task once with everything open. Wardyn watches what it actually did and writes the least-privilege policy. Replay it confined to prove the policy is enough.'
        >
          <RecordPane
            ws={ws}
            notice={recordNotice}
            launch={recordLaunch}
            busyTask={recordBusyTask}
            modelReady={llmReady}
            hostClasses={hostClasses}
            onRecord={(name) => void doRecord(name, false)}
            onReplayConfined={(name) => void doRecord(name, true)}
            onDoneRecording={(runId) => void doneRecording(runId)}
            onPromoteEgress={requestPromoteEgress}
            onApproveHosts={requestApproveHosts}
            onOpenProfile={(runId, suggested) => {
              setProfileRunId(runId);
              setProfileName(suggested);
            }}
          />
        </DetailSectionCard>

        <AllowedHostsCard ws={ws} onWorkspaceUpdated={setWs} />
        <DeniedHostsCard ws={ws} onWorkspaceUpdated={setWs} />
      </div>

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
        selectable={pendingConfirm?.selectable}
        onOpenChange={(o) => !o && setPendingConfirm(null)}
        onConfirm={(approved) => {
          const run = pendingConfirm?.run;
          setPendingConfirm(null);
          run?.(approved);
        }}
      />
    </div>
  );
}
