/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RUN DETAIL — the addressable lifecycle hub at /runs/:id. Rendered inside the
// AppShell outlet (main content only). Replaces the old slide-over Sheet. Tabs:
// Overview / Approvals / Audit / Recording, all driven by REAL data (getRun,
// getGrants, getEgress, listApprovals, listAudit, getRecording).
import * as React from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import {
  ArrowRight,
  Check,
  LayoutDashboard,
  Loader2,
  ScrollText,
  ShieldCheck,
  SquareTerminal,
} from "lucide-react";
import { toast } from "sonner";
import {
  canDecideApproval,
  decisionArgs,
  runHasWorkspace,
  type AgentRun,
  type ApprovalRequest,
  type ApprovalScope,
  type AuditEvent,
  type CredentialGrant,
  type EgressDecision,
  type Recording,
} from "../../lib/types";
import { isTerminalRunState } from "../../lib/types";
import { runs as runsApi } from "../../lib/api/runs";
import { approvals as approvalsApi } from "../../lib/api/approvals";
import { audit as auditApi, egressFromAudit, exitCodeFromAudit } from "../../lib/api/audit";
import { LIST_LIMIT } from "../../lib/api/core";
import { recordings as recordingsApi } from "../../lib/api/recordings";
import { health } from "../../lib/api/health";
import { usePoll } from "../../lib/use-poll";
import { useCopyToClipboard } from "../../lib/use-copy-to-clipboard";
import { absoluteTime, clockTime, getErrorMessage, relativeTime } from "../../lib/format";
import { Button } from "../ui/button";
import { Label } from "../ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "../ui/tabs";
import {
  ActorTypeChip,
  ApprovalKindChip,
  ApprovalStateBadge,
  Chip,
} from "../wardyn/primitives";
import { JsonBlock } from "../wardyn/code-block";
import { EmptyState, ErrorState, TableSkeleton, TruncatedNote } from "../wardyn/states";
import { TerminalPlayer } from "../wardyn/terminal-player";
import { AttachTerminal } from "../attach-terminal";
import { LiveApprovals, isHeld } from "../wardyn/live-approvals";
import { ReasonDialog } from "../wardyn/reason-dialog";
import { useOperator, usePrincipal } from "../wardyn/operator-context";
import {
  OPERATOR_ONLY_REASON,
  RUN_COCKPIT,
  RUN_MODE,
  VIEWER_APPROVAL_BLOCKS_NOTE,
  approvalScopeBadge,
} from "../wardyn/copy";
import { SummaryHeader } from "./run-detail-summary-header";
import { RunDetailCommandBar } from "./run-detail-command-bar";
import { RunCanvas } from "./run-detail/canvas";
import type { WidgetContext } from "./run-detail/widget-registry";

// Live refresh cadence for a non-terminal run's detail.
const DETAIL_POLL_MS = 4000;

type Tab = "overview" | "approvals" | "audit" | "recording";

export function RunDetailScreen() {
  const { id = "" } = useParams();
  const navigate = useNavigate();

  const [run, setRun] = React.useState<AgentRun | null | undefined>(undefined);
  const [grants, setGrants] = React.useState<CredentialGrant[]>([]);
  const [egress, setEgress] = React.useState<EgressDecision[]>([]);
  const [approvals, setApprovals] = React.useState<ApprovalRequest[]>([]);
  const [audit, setAudit] = React.useState<AuditEvent[]>([]);
  // W21-S1-5: the run's session.recording events, indexed SEPARATELY from the
  // general audit trail — that trail is fetched oldest-first with a hard
  // 1000-row cap (LIST_LIMIT), so a chatty run's earlier session.recording
  // events can crowd out later ones (or vice versa: an early one falls off)
  // before the recording picker ever sees them. A tiny second, filtered
  // fetch spends its own 1000-row budget on just this action.
  const [recordingAudit, setRecordingAudit] = React.useState<AuditEvent[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [tab, setTab] = React.useState<Tab>("overview");

  // Recording is fetched lazily the first time the Recording tab opens.
  const [recording, setRecording] = React.useState<Recording | null>(null);
  const [recState, setRecState] = React.useState<"idle" | "loading" | "error" | "ready">("idle");
  // Which cast to replay: the run's own (stored under the bare run id) or one
  // interactive attach session (the composite `<run-id>~<session-uuid>` key).
  const [recKey, setRecKey] = React.useState(id);
  // W21-S1-7: /healthz's components.recording — "none" means this
  // deployment's recording store never came up (stock Helm install:
  // persistence off), so a missing cast is a deployment fact, not "this run
  // happened not to get one". A boot-time fact; read once.
  const [recordingDisabled, setRecordingDisabled] = React.useState(false);
  React.useEffect(() => {
    health.health().then((h) => {
      if (h.components?.recording?.selected === "none") setRecordingDisabled(true);
    });
  }, []);

  const { copied, copyAsync } = useCopyToClipboard(1400);
  const [decide, setDecide] = React.useState<{
    id: string;
    action: "approve" | "deny";
    kind: ApprovalRequest["kind"];
  } | null>(null);

  // Core fetch — run + its grants, egress, approvals, and audit trail.
  const load = React.useCallback(
    (foreground: boolean) => {
      if (!id) return;
      if (foreground) setStatus("loading");
      // Egress is derived from the same audit events we already fetch here — call
      // egressFromAudit(a) instead of api.getEgress (which would re-fetch /audit).
      Promise.all([
        runsApi.getRun(id),
        runsApi.getGrants(id),
        approvalsApi.listApprovals(""),
        auditApi.listAudit(id),
        auditApi.listAudit(id, "session.recording"),
      ])
        .then(([r, g, allApprovals, a, recA]) => {
          setRun(r ?? null);
          setGrants(g);
          setEgress(egressFromAudit(a));
          setApprovals(allApprovals.filter((x) => x.run_id === id));
          setAudit(a);
          setRecordingAudit(recA);
          setStatus("ready");
        })
        .catch(() => {
          // Foreground load shows the error state; a background poll blip keeps
          // last-good data silently (matches the Runs board) rather than toasting
          // every DETAIL_POLL_MS tick during a control-plane hiccup.
          if (foreground) setStatus("error");
        });
    },
    [id],
  );

  React.useEffect(() => {
    setRun(undefined);
    setStatus("loading");
    // fix: reset recording state on run-id change too, or the Recording
    // tab kept showing the PREVIOUS run's cast (labelled as this run) until
    // something else happened to touch recState — the lazy-load effect below
    // only fetches when recState === "idle", so a stale "ready"/"error" from
    // the last run id blocked the refetch entirely.
    setRecording(null);
    setRecState("idle");
    setRecKey(id);
    load(true);
  }, [id, load]);

  const terminal = run ? isTerminalRunState(run.state) : true;
  usePoll(() => load(false), DETAIL_POLL_MS, terminal);

  // Lazy recording load on first Recording-tab open (and on each session pick,
  // which resets recState to "idle").
  //
  // ALSO for a finished run sitting on Overview: its terminal pane replays the
  // cast in place (design board 2d's fourth state), so the fetch can no longer
  // be keyed on the Recording tab alone. Still lazy — a LIVE run on Overview
  // fetches nothing, which is the common case.
  const wantsRecording = tab === "recording" || (tab === "overview" && terminal && !!run);
  React.useEffect(() => {
    if (!wantsRecording || !id || recState !== "idle") return;
    setRecState("loading");
    recordingsApi
      .getRecording(id, recKey || id)
      .then((rec) => {
        setRecording(rec ?? null);
        setRecState("ready");
      })
      .catch(() => setRecState("error"));
  }, [wantsRecording, id, recKey, recState]);

  const copyLink = () => {
    const url = `${window.location.origin}/runs/${encodeURIComponent(id)}`;
    // Only confirm success if the write actually resolves — writeText rejects
    // asynchronously (a sync try/catch misses it), and navigator.clipboard is
    // undefined in insecure contexts — so a bare success toast would lie.
    copyAsync(url).then((ok) => {
      if (ok) toast.success("Link copied");
      else toast.error("Couldn't copy the link — copy it from the address bar.");
    });
  };

  const kill = async () => {
    try {
      await runsApi.killRun(id);
      toast.success(`Kill requested for ${id}`);
    } catch (err) {
      toast.error(`Failed to kill ${id}`, {
        description: getErrorMessage(err),
      });
    } finally {
      load(false);
    }
  };

  const submitDecision = async (reason: string, scope: ApprovalScope, until?: string): Promise<boolean> => {
    if (!decide) return false;
    try {
      const args = decisionArgs(scope, until);
      if (decide.action === "approve") await approvalsApi.approve(decide.id, reason, ...args);
      else await approvalsApi.deny(decide.id, reason, ...args);
      toast.success(decide.action === "approve" ? "Request approved" : "Request denied");
      setDecide(null);
      load(false);
      return true;
    } catch (err) {
      toast.error(decide.action === "approve" ? "Failed to approve" : "Failed to deny", {
        description: getErrorMessage(err),
      });
      return false;
    }
  };

  // ----- top-level states -----
  const pending = approvals.filter((a) => a.state === "PENDING");

  // THE PAGE DOES NOT SCROLL. `h-full min-h-0 flex flex-col` fills app-shell's
  // <main> exactly — main is flex-1 inside a h-screen column, so its height is
  // definite and this resolves against it; overflow-y-auto up there then never
  // fires. Measured in a browser before this was written (1280x720: main
  // scrollHeight === clientHeight, terminal body clipping 6000px into 464px).
  // That is why app-shell.tsx needed no change: every OTHER screen still
  // scrolls exactly as it did.
  //
  // The non-Overview tabs DO scroll, individually — Audit renders up to 1000
  // rows and has to go somewhere.
  return (
    <div className="flex h-full min-h-0 min-w-0 flex-col">
      {status === "loading" ? (
        <div className="space-y-4 p-6">
          <div className="rounded-xl border border-border bg-card p-5">
            <div className="h-6 w-72 animate-pulse rounded bg-muted" />
            <div className="mt-3 h-4 w-96 animate-pulse rounded bg-muted" />
          </div>
          <div className="rounded-xl border border-border bg-card">
            <TableSkeleton rows={6} cols={3} />
          </div>
        </div>
      ) : status === "error" ? (
        <div className="m-6 rounded-xl border border-border bg-card">
          <ErrorState onRetry={() => load(true)} />
        </div>
      ) : !run ? (
        <div className="m-6 rounded-xl border border-border bg-card">
          <EmptyState
            icon={ScrollText}
            title="Run not found"
            description="This run may have been archived or deleted, the link is stale, or you don't have access to it."
            action={
              <Button variant="outline" onClick={() => navigate("/runs")}>
                Back to Runs
              </Button>
            }
          />
        </div>
      ) : (
        <Tabs
          value={tab}
          onValueChange={(v) => setTab(v as Tab)}
          className="flex min-h-0 flex-1 flex-col"
        >
          <SummaryHeader
            run={run}
            terminal={terminal}
            exitCode={exitCodeFromAudit(audit)}
            pendingApprovalCount={pending.length}
            sandboxHeld={pending.some(isHeld)}
            onCopyLink={copyLink}
            linkCopied={copied}
            onKill={kill}
          />

          <RunDetailCommandBar
            tabs={
              <TabsList className="h-7 bg-transparent p-0">
                <TabsTrigger value="overview" className="h-7 gap-1.5 text-xs">
                  <LayoutDashboard className="size-3.5" /> Overview
                </TabsTrigger>
                <TabsTrigger value="approvals" className="h-7 gap-1.5 text-xs">
                  <ShieldCheck className="size-3.5" /> Approvals
                  {pending.length > 0 && (
                    <span className="rounded-full bg-warning-subtle px-1.5 text-[0.6563rem] font-semibold text-warning">
                      {pending.length}
                    </span>
                  )}
                </TabsTrigger>
                <TabsTrigger value="audit" className="h-7 gap-1.5 text-xs">
                  <ScrollText className="size-3.5" /> Audit
                </TabsTrigger>
                <TabsTrigger value="recording" className="h-7 gap-1.5 text-xs">
                  <SquareTerminal className="size-3.5" /> Recording
                </TabsTrigger>
              </TabsList>
            }
          />

          {/* Overview is the cockpit: it fills, and the CANVAS owns the only
              scroll inside it (run-detail/canvas.tsx) — <main> still never
              scrolls, which e2e asserts. */}
          <TabsContent value="overview" className="mt-0 flex min-h-0 flex-1 flex-col">
            <Cockpit
              run={run}
              terminal={terminal}
              grants={grants}
              egress={egress}
              audit={audit}
              pending={pending}
              recording={recording}
              recState={recState}
              recordingDisabled={recordingDisabled}
              onGoAudit={() => setTab("audit")}
              onGoRecording={() => setTab("recording")}
            />
          </TabsContent>

          <TabsContent value="approvals" className="scroll-thin mt-0 min-h-0 flex-1 overflow-y-auto p-4">
            <ApprovalsTab
              approvals={approvals}
              onDecide={(approvalId, action, kind) => setDecide({ id: approvalId, action, kind })}
            />
          </TabsContent>

          <TabsContent value="audit" className="scroll-thin mt-0 min-h-0 flex-1 overflow-y-auto p-4">
            <AuditTab events={audit} runId={run.id} />
          </TabsContent>

          <TabsContent value="recording" className="scroll-thin mt-0 min-h-0 flex-1 overflow-y-auto p-4">
            <RecordingTab
              state={recState}
              recording={recording}
              recordingDisabled={recordingDisabled}
              runId={id}
              sessions={attachSessions(recordingAudit)}
              selected={recKey || id}
              onSelect={(key) => {
                setRecKey(key);
                setRecState("idle");
              }}
              onRetry={() => setRecState("idle")}
            />
          </TabsContent>
        </Tabs>
      )}

      <ReasonDialog
        prompt={decide}
        // Always is greyed out when THIS run resolves to no onboarded
        // workspace (Phase 1e's read-only denormalization) — see
        // reason-dialog.tsx's own doc for why the default is false.
        // runHasWorkspace also covers workspace_id, not just workspace_ids:
        // a record/verify step run carries the former only.
        hasWorkspace={!!run && runHasWorkspace(run)}
        onClose={() => setDecide(null)}
        onSubmit={submitDecision}
      />
    </div>
  );
}

// ---------------------------------------------------------------------------
// Cockpit — the Overview tab. THE TERMINAL IS THE PAGE.
//
// What this replaced: a two-column document that spent its hero on a Run
// timeline (audit.slice(-12)) and put the live terminal below it at h-[70vh],
// so on a 1080p display the prompt line sat ~1,800px down. The timeline is
// GONE — the Audit tab owns the trail — and the terminal now fills the pane.
//
// Layout: phase 2 replaced the fixed terminal-1fr + 400px-rail grid with a
// CONFIGURABLE canvas (design board 2b) — see run-detail/canvas.tsx. This
// component's whole job now is to assemble the WidgetContext every widget
// reads from, including the terminal hero itself: the canvas PLACES the
// terminal, it does not build it, because building it needs the attach /
// recording / approvals graph that lives here.
// ---------------------------------------------------------------------------
function Cockpit({
  run,
  terminal,
  grants,
  egress,
  audit,
  pending,
  recording,
  recState,
  recordingDisabled,
  onGoAudit,
  onGoRecording,
}: {
  run: AgentRun;
  terminal: boolean;
  grants: CredentialGrant[];
  egress: EgressDecision[];
  audit: AuditEvent[];
  /** This run's PENDING approvals — only used to decide whether the viewer
   *  note applies; the decision surface itself is LiveApprovals' own poll. */
  pending: ApprovalRequest[];
  recording: Recording | null;
  recState: "idle" | "loading" | "error" | "ready";
  recordingDisabled: boolean;
  onGoAudit: () => void;
  onGoRecording: () => void;
}) {
  const operator = useOperator();
  const principal = usePrincipal();
  const viewerBlocked =
    pending.length > 0 && !operator && !pending.some((p) => canDecideApproval(false, p.kind));

  // The terminal widget's contents. Unchanged from the fixed-rail cockpit: the
  // session, and directly beneath it the approval that is HOLDING the session —
  // not in a sidebar, not a toast, because the person who has to decide it is
  // already looking here. Interactive OR autonomous, as long as the run is live.
  const terminalPane = (
    <>
      <TerminalPane
        run={run}
        terminal={terminal}
        recording={recording}
        recState={recState}
        recordingDisabled={recordingDisabled}
        onGoRecording={onGoRecording}
      />
      {run.state === "RUNNING" && (
        <div className="shrink-0 space-y-2 pt-2.5">
          {/* Said once, where a viewer actually feels the consequence. This
              used to be half of the Overview's pending-approval banner; the
              banner is gone (the command bar states the count and the strip
              below is the decision surface), but its VIEWER half carries
              information nothing else on the page does — that the run is
              stopped and they personally cannot unstick it. Same
              canDecideApproval kind-question the banner asked: the page is
              already owner-scoped, so a member here owns every approval
              shown; egress_domain is theirs to decide, credential and
              tool_call stay admin-only regardless. */}
          {viewerBlocked && (
            <p className="rounded-lg border border-border bg-muted/40 px-2.5 py-2 text-[0.75rem] leading-relaxed text-muted-foreground">
              {VIEWER_APPROVAL_BLOCKS_NOTE}
            </p>
          )}
          <LiveApprovals runId={run.id} hasWorkspace={runHasWorkspace(run)} />
        </div>
      )}
    </>
  );

  const ctx: WidgetContext = {
    run,
    finished: terminal,
    principal,
    grants,
    egress,
    audit,
    onGoAudit,
    terminalPane,
  };

  return <RunCanvas ctx={ctx} />;
}

// The Terminal widget's run situations (design board 2d). States 1 and 2
// (driving / held by another client) live INSIDE AttachTerminal, which is the
// only thing that knows the socket's attach mode. This picks between the three
// situations the PARENT can tell apart, which is a question about the run, not
// about the socket.
function TerminalPane({
  run,
  terminal,
  recording,
  recState,
  recordingDisabled,
  onGoRecording,
}: {
  run: AgentRun;
  terminal: boolean;
  recording: Recording | null;
  recState: "idle" | "loading" | "error" | "ready";
  recordingDisabled: boolean;
  onGoRecording: () => void;
}) {
  const operator = useOperator();
  const principal = usePrincipal();
  // Same owner-or-admin predicate AttachTerminal gates its own connect on, and
  // the same one the command bar's "attachable" chip claims — all three must
  // agree or the page promises a terminal it then refuses to open.
  const canAttach = operator || (!!run.created_by && run.created_by === principal);
  const attachable = !!run.interactive && run.state === "RUNNING" && canAttach;

  if (attachable) {
    // fill: the pane owns the height. h-[70vh] was a guess that predates this
    // layout and stays the default for every other mount site.
    return <AttachTerminal fill runId={run.id} createdBy={run.created_by} />;
  }

  // Finished run: the pane becomes the replay surface in place rather than a
  // dead box. The Recording TAB is unchanged and remains the full surface
  // (session picker, disabled-store explanation, retry).
  if (terminal) {
    return (
      <PaneFrame title="Recording" chip={RUN_COCKPIT.finishedReplay}>
        {recState === "ready" && recording ? (
          // The player sizes itself with fit:"width" and takes its height from
          // the cast's rows, so it cannot flex — give it its own scroll box
          // rather than letting it push the page.
          <div className="scroll-thin min-h-0 flex-1 overflow-auto p-2">
            <TerminalPlayer recording={recording} />
          </div>
        ) : (
          <PaneNotice
            text={
              recState === "loading" || recState === "idle"
                ? RUN_COCKPIT.recordingLoading
                : recordingDisabled
                  ? RUN_COCKPIT.recordingDisabled
                  : RUN_COCKPIT.recordingMissing
            }
            action={
              <button onClick={onGoRecording} className="text-[0.75rem] font-medium text-primary hover:underline">
                Open the Recording tab →
              </button>
            }
          />
        )}
      </PaneFrame>
    );
  }

  // Live but not drivable: an autonomous run execs the agent directly, so there
  // is no PTY to type into. (A live interactive run the caller may NOT attach to
  // lands here too — the honest thing, since the alternative is a terminal that
  // opens and immediately refuses.)
  return (
    // "Terminal" vs "Output" is not decoration — the board uses them for two
    // different situations. An interactive run HAS a PTY (you just may not
    // drive this one); an autonomous run has none to type into at all, which
    // is why that tile tails output instead of offering a prompt.
    <PaneFrame
      title={run.interactive ? "Terminal" : "Output"}
      chip={run.interactive ? undefined : RUN_COCKPIT.autonomous}
    >
      <PaneNotice
        text={
          run.interactive
            ? OPERATOR_ONLY_REASON
            : RUN_MODE.autonomous.blurb
        }
        action={
          <button onClick={onGoRecording} className="text-[0.75rem] font-medium text-primary hover:underline">
            Watch the captured session →
          </button>
        }
      />
    </PaneFrame>
  );
}

// The non-attached pane, styled as the terminal frame it stands in for so the
// hero keeps its shape across all four run situations.
function PaneFrame({
  title,
  chip,
  children,
}: {
  title: string;
  chip?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden rounded-lg border border-border bg-[#0d1117]">
      <div className="flex h-9 shrink-0 items-center gap-2 border-b border-border bg-card/60 px-3">
        <SquareTerminal className="size-3.5 text-muted-foreground" aria-hidden />
        <span className="label-eyebrow">{title}</span>
        {chip && (
          <Chip tone="neutral" className="font-mono text-[0.625rem]">
            {chip}
          </Chip>
        )}
      </div>
      {children}
    </div>
  );
}

function PaneNotice({ text, action }: { text: string; action?: React.ReactNode }) {
  return (
    <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-2 p-6 text-center">
      <p className="max-w-md text-sm text-muted-foreground">{text}</p>
      {action}
    </div>
  );
}

// Every human attach session is recorded and masked, but under a COMPOSITE cast
// key the console never asked for — so they were write-only. There is no
// list-casts endpoint (and no Store.List to add one on): the index is a
// session.recording-FILTERED audit fetch (W21-S1-5) — not the general trail,
// whose own 1000-row cap a chatty run can blow through — where the event's
// TARGET is that very key.
function attachSessions(audit: AuditEvent[]): AuditEvent[] {
  return audit.filter((e) => e.action === "session.recording" && e.outcome === "success" && e.target);
}

// ---------------------------------------------------------------------------
// Approvals tab (this run's approvals)
// ---------------------------------------------------------------------------
function ApprovalsTab({
  approvals,
  onDecide,
}: {
  approvals: ApprovalRequest[];
  onDecide: (id: string, action: "approve" | "deny", kind: ApprovalRequest["kind"]) => void;
}) {
  const operator = useOperator();
  if (approvals.length === 0) {
    return (
      <div className="rounded-xl border border-border bg-card">
        <EmptyState
          icon={ShieldCheck}
          title="No approvals for this run"
          description="Credential, egress, and tool-call requests for this run will appear here."
        />
      </div>
    );
  }
  return (
    <div className="flex max-w-3xl flex-col gap-3">
      {approvals.map((a) => {
        const pending = a.state === "PENDING";
        // Owner-scoped page (getRunAuthorized) — canDecideApproval only needs
        // the KIND question: egress_domain is a member act on an owned run,
        // credential/tool_call stay admin-only regardless (see its doc).
        const canDecide = canDecideApproval(operator, a.kind);
        const scopeBadge = approvalScopeBadge(a);
        return (
          <div key={a.id} className="rounded-xl border border-border bg-card p-4">
            <div className="flex flex-wrap items-center gap-2">
              <ApprovalKindChip kind={a.kind} />
              <ApprovalStateBadge state={a.state} />
              <span className="ml-auto text-xs text-muted-foreground" title={a.requested_at}>
                requested {relativeTime(a.requested_at)}
              </span>
            </div>
            <div className="mt-3">
              <Label className="text-[0.6875rem] uppercase tracking-wide text-muted-foreground">
                Requested scope
              </Label>
              <JsonBlock value={a.requested_scope} className="mt-1.5" />
            </div>
            {a.decided_by && (
              <div className="mt-2 text-xs text-muted-foreground">
                Decided by <span className="text-foreground">{a.decided_by}</span>
                {/* Scope badge (Phase 0 §6) — the console's DecidedRow shows
                    the same fact; without it here the cockpit would show a
                    decided egress row and the console would show it grew a
                    scope, for the SAME approval. */}
                {scopeBadge && <> · {scopeBadge}</>}
                {a.reason && <> · {a.reason}</>}
              </div>
            )}
            {pending && (
              <div className="mt-3 flex items-center justify-end gap-2 border-t border-border pt-3">
                {!canDecide && <Chip tone="neutral">{OPERATOR_ONLY_REASON}</Chip>}
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => onDecide(a.id, "deny", a.kind)}
                  disabled={!canDecide}
                >
                  Deny
                </Button>
                <Button
                  size="sm"
                  variant="info"
                  onClick={() => onDecide(a.id, "approve", a.kind)}
                  disabled={!canDecide}
                >
                  <Check className="size-4" /> Approve
                </Button>
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Audit tab (this run's events)
// ---------------------------------------------------------------------------
function AuditTab({ events, runId }: { events: AuditEvent[]; runId: string }) {
  return (
    <div className="max-w-4xl">
      <div className="mb-3 flex items-center gap-2 text-xs text-muted-foreground">
        <ScrollText className="size-3.5" />
        Append-only · {events.length} event{events.length === 1 ? "" : "s"} for this run
        {/* W25-W25.2-3: carry the run. A bare /audit is permanently EMPTY for a
            member — the server scopes non-admins to ?run_id= of a run they own
            (internal/api/audit.go handleQueryAudit) — so the unqualified link
            dropped them on a feed that can never fill. */}
        <Link
          to={`/audit?run_id=${runId}`}
          className="ml-1 inline-flex items-center gap-1 text-primary hover:underline"
        >
          open full Audit <ArrowRight className="size-3" />
        </Link>
      </div>
      {/* W17-S1-3: the per-run fetch (auditApi.listAudit) is capped at
          LIST_LIMIT/auditPerRunDefaultLimit and returned OLDEST-first — a
          chatty run's late events silently fall off the end with no cue. */}
      <TruncatedNote count={events.length} cap={LIST_LIMIT}>
        Showing the first {LIST_LIMIT} events for this run (truncated, oldest-first) — later events may
        be missing.
      </TruncatedNote>
      {events.length === 0 ? (
        <div className="rounded-xl border border-border bg-card">
          <EmptyState icon={ScrollText} title="No events yet" description="This run has not recorded any audit events." />
        </div>
      ) : (
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          <div className="divide-y divide-border">
            {events.map((e) => (
              <div key={e.id} className="flex items-center gap-3 px-4 py-2.5">
                <span
                  className="w-[68px] shrink-0 font-mono text-[0.6875rem] text-muted-foreground"
                  title={absoluteTime(e.time)}
                >
                  {clockTime(e.time)}
                </span>
                <ActorTypeChip type={e.actor_type} />
                <span className="w-[190px] shrink-0 truncate font-mono text-[0.75rem] text-muted-foreground" title={e.action}>
                  {e.action}
                </span>
                <span className="min-w-0 flex-1 truncate text-[0.7813rem] text-foreground" title={e.target}>
                  {e.target || "—"}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Recording tab
// ---------------------------------------------------------------------------
function RecordingTab({
  state,
  recording,
  recordingDisabled,
  runId,
  sessions,
  selected,
  onSelect,
  onRetry,
}: {
  state: "idle" | "loading" | "error" | "ready";
  recording: Recording | null;
  /** W21-S1-7: this deployment's recording store never came up — a missing
   *  cast means "it can't", not "it hasn't yet". */
  recordingDisabled: boolean;
  runId: string;
  // The run's interactive attach sessions (session.recording audit events).
  sessions: AuditEvent[];
  selected: string;
  onSelect: (key: string) => void;
  onRetry: () => void;
}) {
  return (
    <div className="max-w-4xl">
      {/* The picker sits ABOVE the body on purpose: a run whose OWN cast is
          missing still has to be able to reach its attach sessions. */}
      {sessions.length > 0 && (
        <div className="mb-3 flex items-center gap-2">
          <span className="text-xs text-muted-foreground">Session</span>
          <Select value={selected} onValueChange={onSelect}>
            <SelectTrigger size="sm" className="w-[280px]" aria-label="Recorded session">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={runId}>Agent session</SelectItem>
              {sessions.map((e) => (
                <SelectItem key={e.id} value={e.target!}>
                  Attached {clockTime(e.time)} · {e.actor}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      )}

      {state === "loading" || state === "idle" ? (
        <div className="flex h-[360px] items-center justify-center rounded-xl border border-border bg-card">
          <Loader2 className="size-5 animate-spin text-muted-foreground" />
        </div>
      ) : state === "error" ? (
        <div className="rounded-xl border border-border bg-card">
          <ErrorState message="Couldn't load this run's recording." onRetry={onRetry} />
        </div>
      ) : !recording ? (
        <div className="rounded-xl border border-border bg-card">
          <EmptyState
            icon={SquareTerminal}
            title={recordingDisabled ? "Session recording is disabled on this deployment" : "No recording available"}
            description={
              recordingDisabled
                ? "No run on this server captures one — set persistence.enabled (Helm) or WARDYN_RECORDING_DIR to turn it on."
                : "This run has no captured terminal session. A recording is produced once an agent process runs in the sandbox."
            }
          />
        </div>
      ) : (
        <>
          <TerminalPlayer recording={recording} />
          <div className="mt-2 text-xs text-muted-foreground">
            Recorded when the run's runner supports session capture ·{" "}
            <Link to="/recordings" className="text-primary hover:underline">
              Recordings library
            </Link>
          </div>
        </>
      )}
    </div>
  );
}
