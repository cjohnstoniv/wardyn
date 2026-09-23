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
  LayoutDashboard,
  Loader2,
  RotateCw,
  ScrollText,
  ShieldCheck,
  Sparkles,
  SquareTerminal,
} from "lucide-react";
import { toast } from "sonner";
import {
  canDecideAdoCapability,
  canDecideApproval,
  decisionArgs,
  isAdoCapabilityRequest,
  isAdoConsentRequest,
  runHasWorkspace,
  type ApprovalRequest,
  type ApprovalScope,
  type AuditEvent,
  type CredentialGrant,
  type DecisionOptions,
  type EgressDecision,
  type Recording,
  type RunDetail,
} from "../../lib/types";
import { isTerminalRunState } from "../../lib/types";
import { runs as runsApi } from "../../lib/api/runs";
import { approvals as approvalsApi } from "../../lib/api/approvals";
import {
  audit as auditApi,
  createRequestFromAudit,
  egressFromAudit,
  exitCodeFromAudit,
} from "../../lib/api/audit";
import { LIST_LIMIT } from "../../lib/api/core";
import { recordings as recordingsApi } from "../../lib/api/recordings";
import { useRecordingDisabled } from "../../lib/hooks/use-recording-disabled";
import { usePoll } from "../../lib/use-poll";
import { useCopyToClipboard } from "../../lib/use-copy-to-clipboard";
import { absoluteTime, clockTime, getErrorMessage } from "../../lib/format";
import { Button } from "../ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "../ui/tabs";
import { ActorTypeChip } from "../wardyn/primitives";
import { AuditDecision, RuleSourceChip, toolRuleDecision } from "../wardyn/audit-decision";
import { EmptyState, ErrorState, TableSkeleton, TruncatedNote } from "../wardyn/states";
import { TerminalPlayer } from "../wardyn/terminal-player";
import { LiveApprovals, isHeld } from "../wardyn/live-approvals";
import { ReasonDialog } from "../wardyn/reason-dialog";
import { APPROVALS } from "../../lib/approvals-copy";
import { useOperator, usePrincipal, useSecurityOperator } from "../wardyn/operator-context";
import {
  RECORDING_DISABLED_DESC,
  RECORDING_DISABLED_TITLE,
  RUN_COCKPIT,
  VIEWER_APPROVAL_BLOCKS_NOTE,
} from "../wardyn/copy";
import { ProfileReview } from "./profile-review";
import { SummaryHeader } from "./run-detail-summary-header";
import { ApprovalsTab } from "./run-detail-approvals-tab";
import { RunDetailCommandBar } from "./run-detail-command-bar";
import { RunCanvas } from "./run-detail/canvas";
import { RunFailureBlock } from "./run-detail/failure-block";
import { LoginSandboxNote } from "./run-detail/login-sandbox-note";
import { TerminalPane } from "./run-detail/terminal-notice";
import { sessionOptionLabel, RECORDING_MISSING_SESSION_TITLE, RECORDING_MISSING_SESSION_BODY } from "./run-detail/recording-tab-copy";
import { cloneFromAudit, CLONE_UNREADABLE } from "./new-run/wizard-types";
import type { WidgetContext } from "./run-detail/widget-registry";

// Live refresh cadence for a non-terminal run's detail.
const DETAIL_POLL_MS = 4000;

type Tab = "overview" | "approvals" | "audit" | "recording";

export function RunDetailScreen() {
  const { id = "" } = useParams();
  const navigate = useNavigate();

  const [run, setRun] = React.useState<RunDetail | null | undefined>(undefined);
  const [grants, setGrants] = React.useState<CredentialGrant[]>([]);
  const [egress, setEgress] = React.useState<EgressDecision[]>([]);
  const [approvals, setApprovals] = React.useState<ApprovalRequest[]>([]);
  const [audit, setAudit] = React.useState<AuditEvent[]>([]);
  // The run's session.recording events, indexed SEPARATELY from the
  // general audit trail — that trail is fetched oldest-first with a hard
  // 1000-row cap (LIST_LIMIT), so a chatty run's earlier session.recording
  // events can crowd out later ones (or vice versa: an early one falls off)
  // before the recording picker ever sees them. A tiny second, filtered
  // fetch spends its own 1000-row budget on just this action.
  const [recordingAudit, setRecordingAudit] = React.useState<AuditEvent[]>([]);
  // F6-F2: run.complete/run.kill/run.autostop are the LATEST events on a run's
  // trail — the first ones the 1000-row cap on `audit` above pushes off —
  // scoped-fetched the same way session.recording is, so the exit code and
  // ending derivation stay known past that cap.
  const [endingAudit, setEndingAudit] = React.useState<AuditEvent[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [tab, setTab] = React.useState<Tab>("overview");
  // "Make a policy from this run" — the honest home of "write the policy from
  // what actually happened", now that /runs/new's Record radio (which only ever
  // set allow_all_egress) is gone. Same runId-driven ProfileReview sheet
  // workspace-detail.tsx and setup/demos-step.tsx already mount: it POSTs
  // /runs/{id}/profile, renders the proposal's inline_policy verbatim, and its
  // own "Save as policy" persists it via POST /policies. Local open-state only.
  const [profileRunId, setProfileRunId] = React.useState<string | null>(null);

  // Recording is fetched lazily the first time the Recording tab opens.
  const [recording, setRecording] = React.useState<Recording | null>(null);
  const [recState, setRecState] = React.useState<"idle" | "loading" | "error" | "ready">("idle");
  // Which cast to replay: the run's own (stored under the bare run id) or one
  // interactive attach session (the composite `<run-id>~<session-uuid>` key).
  const [recKey, setRecKey] = React.useState(id);
  // Now the shared hook: the same /healthz read the Recordings
  // library and the New Run rail make. See use-recording-disabled.ts.
  const recordingDisabled = useRecordingDisabled() === true;

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
      // RETURNED, not fired and forgotten: usePoll's in-flight guard waits on
      // this promise, so a control plane slower than DETAIL_POLL_MS costs one
      // outstanding set of requests instead of a new set every 4s (R4-F074).
      // Egress is derived from the same audit events we already fetch here — call
      // egressFromAudit(a) instead of api.getEgress (which would re-fetch /audit).
      // allSettled, NOT all. The PAGE is the run; the other four are panes on
      // it — a single subsidiary rejection under Promise.all would replace the
      // whole cockpit (run state, live terminal, approvals strip and the KILL
      // button for a RUNNING run) with ErrorState's "we couldn't reach the
      // Wardyn control plane", an outage claim that is false when GET
      // /runs/{id} just returned 200. The rejection is routine, not
      // hypothetical: handleListApprovals answers 500 "approval listing is not
      // scoped for members on this backend" on a backend without
      // ApprovalsByRunCreatorPager (internal/api/approvals.go), and a degraded
      // audit store fails listAudit. Each pane keeps its last-good value and
      // the page stays operable.
      return Promise.allSettled([
        runsApi.getRun(id),
        runsApi.getGrants(id),
        // Scoped SERVER-side (?run_id=). Filtering this list in the browser
        // instead drops the run's own approvals once the fleet has more than
        // LIST_LIMIT lifetime rows — see approvals.ts's listApprovals comment
        // and internal/api/approvals.go:56-61.
        approvalsApi.listApprovals("", id),
        auditApi.listAudit(id),
        auditApi.listAudit(id, "session.recording"),
      ])
        .then(([r, g, runApprovals, a, recA]) => {
          if (r.status === "rejected") {
            // The run itself is the one fetch this page cannot render without.
            // Foreground load shows the error state; a background poll blip
            // keeps last-good data silently (matches the Runs board) rather
            // than replacing a live cockpit every DETAIL_POLL_MS during a
            // control-plane hiccup.
            if (foreground) setStatus("error");
            return;
          }
          setRun(r.value ?? null);
          if (g.status === "fulfilled") setGrants(g.value);
          if (a.status === "fulfilled") {
            setEgress(egressFromAudit(a.value));
            setAudit(a.value);
          }
          // The ?run_id= above is what makes this list this run's; the filter
          // is a belt-and-braces no-op kept so a backend that ignored the
          // predicate cannot leak another run's rows onto this page.
          if (runApprovals.status === "fulfilled")
            setApprovals(runApprovals.value.filter((x) => x.run_id === id));
          if (recA.status === "fulfilled") setRecordingAudit(recA.value);
          // R-5: run.complete/run.kill/run.autostop cannot exist for a run
          // that ISN'T terminal — fetching them every DETAIL_POLL_MS tick on
          // a live run would be 3 wasted round-trips per tick, forever. Gated
          // on THIS tick's own fresh state (not a stale last-known ref), so
          // the exact tick a run turns terminal is the one that catches it.
          if (r.value && isTerminalRunState(r.value.state)) {
            Promise.all([
              auditApi.listAudit(id, "run.complete"),
              auditApi.listAudit(id, "run.kill"),
              auditApi.listAudit(id, "run.autostop"),
            ]).then((lists) => setEndingAudit(lists.flat()));
          }
          setStatus("ready");
        })
        .catch(() => {
          // allSettled never rejects, so this is a bug in the block above, not
          // a network answer. Same foreground rule.
          if (foreground) setStatus("error");
        });
    },
    [id],
  );

  React.useEffect(() => {
    setRun(undefined);
    setStatus("loading");
    // Reset recording state on run-id change too: without it, the Recording
    // tab would keep showing the PREVIOUS run's cast (labelled as this run)
    // until something else touched recState — the lazy-load effect below
    // only fetches when recState === "idle", so a stale "ready"/"error" from
    // the last run id would block the refetch entirely.
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
  // F1-F2: without an ordering guard, a slow fetch for an earlier-selected
  // cast (recKey A) could resolve AFTER a later selection (recKey B) and
  // overwrite it, or setState after unmount. NOT a plain `let alive` + cleanup (the
  // sibling pattern in run-context-row.tsx/run-detail-ssh.tsx): this effect's
  // own setRecState("loading") is itself a dependency-array member, so a
  // cleanup tied to every re-run would invalidate the very request it just
  // started. A generation counter only advances when a NEW fetch actually
  // starts, so it survives the effect's own idle->loading->ready churn.
  const recRequest = React.useRef(0);
  React.useEffect(() => {
    if (!wantsRecording || !id || recState !== "idle") return;
    const thisRequest = ++recRequest.current;
    setRecState("loading");
    recordingsApi
      .getRecording(id, recKey || id)
      .then((rec) => {
        if (recRequest.current !== thisRequest) return;
        setRecording(rec ?? null);
        setRecState("ready");
      })
      .catch(() => {
        if (recRequest.current === thisRequest) setRecState("error");
      });
  }, [wantsRecording, id, recKey, recState]);
  // F1-F2's other half: setState after unmount. The counter must also
  // advance on teardown, not only when a new fetch starts — -1 never
  // matches a real (>=1) generation, so any in-flight fetch's callback is
  // permanently a no-op once this component is gone.
  React.useEffect(() => () => { recRequest.current = -1; }, []);

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

  // 0.7.3 F7 / review U-01 — the header's clone door, same cloneFromAudit
  // refusal as the Runs-list kebab (run-card.tsx): no run.create row, no
  // navigation.
  const onClone = () => {
    if (!run) return;
    const prefill = cloneFromAudit(run, audit);
    if (!prefill) {
      toast.warning(CLONE_UNREADABLE);
      return;
    }
    navigate("/runs/new", { state: { prefill } });
  };

  const submitDecision = async (reason: string, scope: ApprovalScope, until?: string): Promise<boolean> => {
    if (!decide) return false;
    try {
      const args = decisionArgs(scope, until);
      if (decide.action === "approve") await approvalsApi.approve(decide.id, reason, ...args);
      else await approvalsApi.deny(decide.id, reason, ...args);
      toast.success(decide.action === "approve" ? APPROVALS.TOAST_APPROVED : APPROVALS.TOAST_DENIED);
      setDecide(null);
      load(false);
      return true;
    } catch (err) {
      toast.error(decide.action === "approve" ? APPROVALS.TOAST_APPROVE_FAILED : APPROVALS.TOAST_DENY_FAILED, {
        description: getErrorMessage(err),
      });
      return false;
    }
  };

  // decideAdoDirect — the Approvals tab's own Azure DevOps decide path (S10
  // round 2, F2). Bypasses ReasonDialog/submitDecision entirely, same reason
  // screens/approvals.tsx's decideAdoDirect does: the card's own control
  // carries no reason field, and `opts` ALWAYS carries an explicit
  // decision_scope — decisionArgs()'s omit-for-"run" shape would collide
  // with adoDecisionRule's different bodyless default (see ado-capability-
  // card.tsx's adoDecisionArgs).
  const decideAdoDirect = async (id: string, approve: boolean, opts: [DecisionOptions]): Promise<void> => {
    try {
      if (approve) await approvalsApi.approve(id, "approved", ...opts);
      else await approvalsApi.deny(id, "denied", ...opts);
      toast.success(approve ? APPROVALS.TOAST_APPROVED : APPROVALS.TOAST_DENIED);
      load(false);
    } catch (err) {
      toast.error(approve ? APPROVALS.TOAST_APPROVE_FAILED : APPROVALS.TOAST_DENY_FAILED, { description: getErrorMessage(err) });
    }
  };

  // decidePushDirect — the Approvals tab's own push_content decide path
  // (#181, review finding 3). Same reason decideAdoDirect bypasses
  // ReasonDialog/submitDecision: the card's own control carries no reason
  // field — but unlike ADO, NO opts at all (decide's rule 4 refuses a
  // decision_scope on this kind).
  const decidePushDirect = async (id: string, approve: boolean): Promise<void> => {
    try {
      if (approve) await approvalsApi.approve(id, "approved");
      else await approvalsApi.deny(id, "denied");
      toast.success(approve ? "Request approved" : "Request denied");
      load(false);
    } catch (err) {
      toast.error(approve ? "Failed to approve" : "Failed to deny", { description: getErrorMessage(err) });
    }
  };

  // ----- top-level states -----
  const pending = approvals.filter((a) => a.state === "PENDING");
  // F6-F2: run.complete/run.kill/run.autostop rows land here even when the
  // capped `audit` trail above dropped them — duplicates are harmless, both
  // derivations below keep the last/first matching row regardless.
  const endingEvents = [...audit, ...endingAudit];

  // The page does not scroll. `h-full min-h-0 flex flex-col` fills
  // app-shell's <main> exactly — main is flex-1 inside a h-screen column, so
  // its height is definite and this resolves against it; overflow-y-auto up
  // there then never fires (measured at 1280x720: main scrollHeight ===
  // clientHeight, terminal body clipping 6000px into 464px). app-shell.tsx
  // needs no change for this — every OTHER screen still scrolls.
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
          <ErrorState
            action={
              <Button variant="outline" size="sm" onClick={() => load(true)}>
                <RotateCw className="size-3.5" /> Retry
              </Button>
            }
          />
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
            exitCode={exitCodeFromAudit(endingEvents)}
            pendingApprovalCount={pending.length}
            sandboxHeld={pending.some(isHeld)}
            // F13 — split, so the AWS-named chip never fires for an Azure
            // DevOps consent row (isAdoConsentRequest is also credential_reauth).
            awaitingReauth={pending.some((p) => p.kind === "credential_reauth" && !isAdoConsentRequest(p))}
            awaitingAdoConsent={pending.some(isAdoConsentRequest)}
            onCopyLink={copyLink}
            linkCopied={copied}
            onKill={kill}
            onClone={onClone}
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
                    <span className="rounded-full bg-warning-subtle px-1.5 text-meta font-semibold text-warning">
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
              audit={endingEvents}
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
              run={run}
              onDecide={(approvalId, action, kind) => setDecide({ id: approvalId, action, kind })}
              onAdoDecide={decideAdoDirect}
              onPushDecide={decidePushDirect}
            />
          </TabsContent>

          <TabsContent value="audit" className="scroll-thin mt-0 min-h-0 flex-1 overflow-y-auto p-4">
            <AuditTab events={audit} runId={run.id} onMakePolicy={() => setProfileRunId(id)} />
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

      <ProfileReview runId={profileRunId} onClose={() => setProfileRunId(null)} />

      <ReasonDialog
        prompt={decide}
        // Always is greyed out when THIS run resolves to no onboarded
        // workspace (a read-only denormalization) — see reason-dialog.tsx's
        // own doc for why the default is false.
        // runHasWorkspace also covers workspace_id, not just workspace_ids:
        // a record/verify step run carries the former only.
        hasWorkspace={!!run && runHasWorkspace(run)}
        onClose={() => setDecide(null)}
        onSubmit={submitDecision}
      />
    </div>
  );
}

// Cockpit — the Overview tab. The terminal is the page: the Audit tab owns
// the event trail, so this component gives the terminal the full pane
// rather than sharing it with a timeline.
//
// Layout: a CONFIGURABLE canvas (design board 2b) — see run-detail/canvas.tsx.
// This component's whole job is to assemble the WidgetContext every widget
// reads from, including the terminal hero itself: the canvas PLACES the
// terminal, it does not build it, because building it needs the attach /
// recording / approvals graph that lives here.
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
  run: RunDetail;
  terminal: boolean;
  grants: CredentialGrant[];
  egress: EgressDecision[];
  audit: AuditEvent[];
  /** This run's PENDING approvals — the viewer note, and (B3) the ONE live
   *  held count every widget reads; the decision surface itself is
   *  LiveApprovals' own poll. */
  pending: ApprovalRequest[];
  recording: Recording | null;
  recState: "idle" | "loading" | "error" | "ready";
  recordingDisabled: boolean;
  onGoAudit: () => void;
  onGoRecording: () => void;
}) {
  const principal = usePrincipal();
  // The run's REQUEST-scoped facts, off its run.create audit row — the only
  // durable record of task_mode, interactive_start, seed_auto_tools and
  // tool_approvals, none of which lands on AgentRun. Read once here; the exec
  // pane reads it below.
  const createRequest = createRequestFromAudit(audit);
  // useSecurityOperator, not useOperator (0.7 §B): this banner says "you can't
  // decide any of these", and authorizeMemberDecision (approvals.go:392)
  // early-returns for the security tier — so a security admin can decide every
  // one of them and must never be told otherwise. The SUPER-only surfaces on
  // this page (attach, take-over) read useOperator in their own components.
  const securityOperator = useSecurityOperator();
  // The SUPER-admin question, for the widget context: ConnectSSHCard reads it
  // itself, and RUN_WIDGETS.ssh.available has to ask the same one.
  const operator = useOperator();
  // "blocked until an admin decides" is FALSE for a re-auth row (UX round B2):
  // no admin decides it, and the person who can fix it is the credential's own
  // owner. The kind is excluded from the predicate rather than the sentence
  // reworded — a run whose ONLY pending row is a re-auth is not blocked on
  // anyone's decision at all, and the strip's own heading says what it needs.
  //
  // S10 round 2 (F2): an Azure DevOps escalation this viewer OWNS is ALSO
  // excluded — canDecideApproval doesn't know the ADO ownership carve-out
  // (canDecideAdoCapability does), so without this a run's own owner read
  // this "you're blocked" note over a card that, two lines below, lets them
  // decide it.
  const isRunOwner = run.created_by === principal;
  const viewerBlocked =
    pending.length > 0 &&
    !securityOperator &&
    pending.some((p) => p.kind !== "credential_reauth") &&
    !pending.some(
      (p) =>
        p.kind !== "credential_reauth" &&
        (canDecideApproval(false, p.kind) || (isAdoCapabilityRequest(p) && canDecideAdoCapability(false, isRunOwner))),
    );

  // The terminal widget's contents. Unchanged from the fixed-rail cockpit: the
  // session, and directly beneath it the approval that is HOLDING the session —
  // not in a sidebar, not a toast, because the person who has to decide it is
  // already looking here. Interactive OR autonomous, as long as the run is live.
  // task_mode lives only in the run.create audit event (request-scoped, never
  // on AgentRun) — this page already holds the full trail, so the pane can
  // speak honestly about a no-harness run for free.
  const execMode = createRequest.task_mode === "exec";
  const terminalPane = (
    <>
      {/* M7(b): above the terminal, because on a run that ended badly the
          replay is not the news — why it ended is. Inside the terminal widget
          rather than beside it so the canvas keeps placing exactly one hero,
          and nothing on this page moves for a run that ended fine (the block
          renders null unless the audit trail says otherwise). The clone door
          lives on the run header instead (0.7.3 F7), a strict superset of
          the states this block explains, so this block takes no onClone. */}
      <LoginSandboxNote run={run} />
      <RunFailureBlock run={run} audit={audit} onGoAudit={onGoAudit} />
      <TerminalPane
        run={run}
        terminal={terminal}
        recording={recording}
        recState={recState}
        recordingDisabled={recordingDisabled}
        onGoRecording={onGoRecording}
        execMode={execMode}
      />
      {run.state === "RUNNING" && (
        <div className="shrink-0 space-y-2 pt-2.5">
          {/* Said once, where a viewer actually feels the consequence: the
              run is stopped and they personally cannot unstick it — the
              command bar states the pending count and the strip below is the
              decision surface, so this is the VIEWER-only half of that
              story. Same canDecideApproval kind-question: the page is
              already owner-scoped, so a member here owns every approval
              shown; egress_domain is theirs to decide, credential and
              tool_call stay admin-only regardless. */}
          {viewerBlocked && (
            <p className="rounded-lg border border-border bg-muted/40 px-2.5 py-2 text-xs leading-relaxed text-muted-foreground">
              {VIEWER_APPROVAL_BLOCKS_NOTE}
            </p>
          )}
          <LiveApprovals runId={run.id} hasWorkspace={runHasWorkspace(run)} run={run} />
        </div>
      )}
    </>
  );

  const ctx: WidgetContext = {
    run,
    finished: terminal,
    principal,
    // The ssh widget's gate is owner-or-admin, like the card it places.
    operator,
    grants,
    egress,
    // B3 — the SAME derivation the command bar's "sandbox held" and the board's
    // card state already use (isHeld, live-approvals.tsx), not a second copy
    // and not a count of audit rows. `egress` above stays the history the rows
    // render; this is the state the alarm chip states.
    heldCount: pending.filter(isHeld).length,
    audit,
    onGoAudit,
    terminalPane,
  };

  return <RunCanvas ctx={ctx} />;
}

// Every human attach session is recorded and masked, but under a COMPOSITE cast
// key the console never asked for — so it is write-only without an index.
// There is no list-casts endpoint (and no Store.List to add one on): the
// index is a session.recording-FILTERED audit fetch — not the
// general trail, whose own 1000-row cap a chatty run can blow through —
// where the event's TARGET is that very key.
function attachSessions(audit: AuditEvent[]): AuditEvent[] {
  return audit.filter((e) => e.action === "session.recording" && e.outcome === "success" && e.target);
}

// Audit tab (this run's events)
function AuditTab({
  events,
  runId,
  onMakePolicy,
}: {
  events: AuditEvent[];
  runId: string;
  onMakePolicy: () => void;
}) {
  return (
    <div className="max-w-4xl">
      <div className="mb-3 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
        <ScrollText className="size-3.5" />
        Append-only · {events.length} event{events.length === 1 ? "" : "s"} for this run
        {/* W25-W25.2-3: carry the run. A bare /audit is permanently EMPTY for a
            member — the server scopes non-admins to ?run_id= of a run they own
            (internal/api/audit.go handleQueryAudit) — so an unqualified link
            would drop them on a feed that can never fill. */}
        <Link
          to={`/audit?run_id=${runId}`}
          className="ml-1 inline-flex items-center gap-1 text-primary hover:underline"
        >
          open full Audit <ArrowRight className="size-3" />
        </Link>
        {/* Beside the record it is synthesized FROM, not on the command bar:
            what this run actually did is the whole basis of the proposal. */}
        <Button variant="outline" size="sm" className="ml-auto h-7" onClick={onMakePolicy}>
          <Sparkles className="size-3.5" /> Make a policy from this run
        </Button>
      </div>
      {/* The per-run fetch (auditApi.listAudit) is capped at
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
                  className="w-[68px] shrink-0 font-mono text-meta text-muted-foreground"
                  title={absoluteTime(e.time)}
                >
                  {clockTime(e.time)}
                </span>
                <ActorTypeChip type={e.actor_type} />
                <span className="w-[190px] shrink-0 truncate font-mono text-xs text-muted-foreground" title={e.action}>
                  {e.action}
                </span>
                {/* A tool call the policy's own tool_rules answered rides an
                    egress.allow/deny row whose target is the CONTROL PLANE —
                    so the row says who decided, as the Audit screen does. */}
                {toolRuleDecision(e) ? (
                  <AuditDecision event={e} className="flex min-w-0 flex-1 items-center gap-2 text-xs" />
                ) : (
                  <span className="flex min-w-0 flex-1 items-center gap-2">
                    <span className="min-w-0 flex-1 truncate text-xs text-foreground" title={e.target}>
                      {e.target || "—"}
                    </span>
                    <RuleSourceChip event={e} />
                  </span>
                )}
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

// Recording tab
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
  /** This deployment's recording store never came up — a missing
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
                  {sessionOptionLabel(e)}
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
          <ErrorState message={RUN_COCKPIT.recordingError} onRetry={onRetry} />
        </div>
      ) : !recording ? (
        <div className="rounded-xl border border-border bg-card">
          <EmptyState
            icon={SquareTerminal}
            title={
              recordingDisabled
                ? RECORDING_DISABLED_TITLE
                // F1-F11: a SPECIFIC attach session's missing cast is not a
                // fact about the whole run — the picker above is already
                // looking at one session, so the empty state must say so too.
                : selected !== runId
                  ? RECORDING_MISSING_SESSION_TITLE
                  : "No recording available"
            }
            description={
              recordingDisabled
                ? RECORDING_DISABLED_DESC
                : selected !== runId
                  ? RECORDING_MISSING_SESSION_BODY
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
