/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RUN DETAIL — the addressable lifecycle hub at /runs/:id. Rendered inside the
// AppShell outlet (main content only). Replaces the old slide-over Sheet. Tabs:
// Overview / Approvals / Policy / Audit / Recording, all driven by REAL data (getRun,
// getGrants, getEgress, listApprovals, listAudit, getRecording).
import * as React from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import {
  ArrowRight,
  FileText,
  LayoutDashboard,
  Logs,
  RotateCw,
  ScrollText,
  ShieldCheck,
  Sparkles,
  SquareTerminal,
} from "lucide-react";
import { toast } from "sonner";
import {
  decisionArgs,
  isAdoConsentRequest,
  runHasWorkspace,
  type ApprovalRequest,
  type ApprovalScope,
  type AuditEvent,
  type DecisionOptions,
  type RunDetail,
} from "../../lib/types";
import { ERASED_VALUE } from "../../lib/types";
import { runs as runsApi } from "../../lib/api/runs";
import { approvals as approvalsApi } from "../../lib/api/approvals";
import { canonicalAuditAction, exitCodeFromAudit } from "../../lib/api/audit";
import { LIST_LIMIT } from "../../lib/api/core";
import { appURL } from "../../lib/base-path";
import { useCopyToClipboard } from "../../lib/use-copy-to-clipboard";
import { absoluteTime, clockTime, getErrorMessage } from "../../lib/format";
import { Button } from "../ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "../ui/tabs";
import { ActorTypeChip } from "../wardyn/primitives";
import {
  AuditDecision,
  ErasedFields,
  isAttributedRefusal,
  RuleSourceChip,
  toolRuleDecision,
} from "../wardyn/audit-decision";
import { PolicyRemedy } from "../wardyn/policy-remedy";
import { newRunHref } from "./new-run-flow/draft/new-run-url";
import { AUDIT } from "./audit-copy";
import { EmptyState, ErrorState, TableSkeleton, TruncatedNote } from "../wardyn/states";
import { isHeld } from "../wardyn/live-approvals";
import { ReasonDialog } from "../wardyn/reason-dialog";
import { APPROVALS } from "../../lib/approvals-copy";
import { usePrincipal, useSecurityOperator } from "../wardyn/operator-context";
import { useConsoleMode } from "../wardyn/console-view";
import { RUN_COCKPIT, RUN_OUTPUT } from "../wardyn/copy";
import { ProfileReview } from "./profile-review";
import { SummaryHeader } from "./run-detail-summary-header";
import { viewerBlockedOnDecision } from "./run-detail/viewer-blocked";
import { ApprovalsTab } from "./run-detail-approvals-tab";
import { RunDetailCommandBar } from "./run-detail-command-bar";
import { LaunchWarningsNote } from "./run-detail/launch-warnings-note";
import { RunLifetimeWithContainment } from "./run-detail/containment-pending";
import { RunEndsRow } from "./run-detail/run-ends-row";
import { PolicyTab } from "./run-detail/policy-tab";
import { POLICY_TAB } from "./run-detail/policy-tab-copy";
import { RecordingTab } from "./run-detail/recording-tab";
import { OutputTab } from "./run-detail/output-tab";
import { cloneFromAudit, CLONE_UNREADABLE } from "./new-run-flow/draft/clone-prefill";
import { launchIntentSource } from "./new-run-flow/steps/shape/shape-sources";
import { useRunDetail, type Tab } from "./run-detail/use-run-detail";
import { Cockpit } from "./run-detail/cockpit";

// The route's component. KEYED by the route id on both run routes (/runs/:id
// and /admin/runs/:id), so moving from run A to run B REMOUNTS the page: A's
// in-flight load, its poll's in-flight guard, its detached fetches and every
// piece of state it held (grants, approvals, recording, ending audit) die with
// the old instance instead of landing on B (#1483). react-router reuses one
// element across a param change, so without the key nothing resets.
// The TabsList is transparent on the page, so the primitive's light-theme active pill (bg-card) vanishes
// into it and no tab reads as selected; give the active tab a visible fill and the rest a muted label (#2911).
const RUN_TAB =
  "h-7 gap-1.5 text-xs text-muted-foreground data-[state=active]:border-border data-[state=active]:bg-muted data-[state=active]:text-foreground";

export function RunDetailScreen() {
  const { id = "" } = useParams();
  return <RunDetailPage key={id} id={id} />;
}

function RunDetailPage({ id }: { id: string }) {
  const navigate = useNavigate();
  const view = useConsoleMode(); // M-7: no relaunch/SSH/credential door in admin view.
  const securityOperator = useSecurityOperator();
  const principal = usePrincipal();

  const {
    run, grants, egress, approvals, audit, auditGap, recordingAudit, status, tab, setTab,
    recording, recState, setRecState, recKey, setRecKey, recordingDisabled,
    load, terminal, pending, endingEvents, held, killAgain, outcomeReady,
  } = useRunDetail(id);

  // "Make a policy from this run" — the honest home of "write the policy from
  // what actually happened", now that /runs/new's Record radio (which only ever
  // set allow_all_egress) is gone. Same runId-driven ProfileReview sheet
  // workspace-detail.tsx and setup/demos-step.tsx already mount: it POSTs
  // /runs/{id}/profile/synthesize, renders the proposal's inline_policy verbatim, and its
  // own "Save as policy" persists it via POST /policies. Local open-state only.
  const [profileRunId, setProfileRunId] = React.useState<string | null>(null);

  const { copyAsync } = useCopyToClipboard(1400);
  const [decide, setDecide] = React.useState<{
    id: string;
    action: "approve" | "deny";
    kind: ApprovalRequest["kind"];
  } | null>(null);

  const copyLink = () => {
    if (!run) return;
    const url = `${window.location.origin}${appURL(`/runs/${encodeURIComponent(run.id)}`)}`;
    // Only confirm success if the write actually resolves — writeText rejects
    // asynchronously (a sync try/catch misses it), and navigator.clipboard is
    // undefined in insecure contexts — so a bare success toast would lie.
    void copyAsync(url).then((ok) => {
      if (ok) toast.success("Link copied");
      else toast.error("Couldn't copy the link — copy it from the address bar.");
    });
  };

  // killId is what the confirm dialog NAMED; this sends exactly that id.
  const kill = async (killId: string) => {
    try {
      await runsApi.killRun(killId);
      toast.success(`Kill requested for ${killId}`);
    } catch (err) {
      toast.error(`Failed to kill ${killId}`, {
        description: getErrorMessage(err),
      });
    } finally {
      void load(false);
    }
  };

  // Rename (#1197 L2): PATCH /runs/{id}/title, then re-fetch so the new
  // title renders in place — no reload, same `load(false)` pattern kill and
  // the decision handlers below already use.
  const rename = async (title: string) => {
    if (!run) return;
    try {
      await runsApi.setTitle(run.id, title);
      toast.success(RUN_COCKPIT.renamed);
    } catch (err) {
      toast.error(`Couldn't rename this run`, { description: getErrorMessage(err) });
    } finally {
      void load(false);
    }
  };

  // The header's clone door. A run that kept its setup opens through Run again;
  // any other run falls back to what its row and audit trail hold, and with no
  // run.create row there is nothing honest to clone: it toasts and stays.
  const onClone = async () => {
    if (!run) return;
    try {
      await launchIntentSource.read(run.id);
      void navigate(newRunHref({ screen: "review", view: "pretty" }, { again: run.id }));
      return;
    } catch {
      // Kept no setup, or it could not be read: the audit trail answers below.
    }
    const prefill = cloneFromAudit(run, audit);
    if (!prefill) {
      toast.warning(CLONE_UNREADABLE);
      return;
    }
    void navigate("/runs/new", { state: { prefill } });
  };

  const submitDecision = async (reason: string, scope: ApprovalScope, until?: string): Promise<boolean> => {
    if (!decide) return false;
    try {
      const args = decisionArgs(scope, until);
      if (decide.action === "approve") await approvalsApi.approve(decide.id, reason, ...args);
      else await approvalsApi.deny(decide.id, reason, ...args);
      toast.success(decide.action === "approve" ? APPROVALS.TOAST_APPROVED : APPROVALS.TOAST_DENIED);
      setDecide(null);
      void load(false);
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
  // card.tsx's adoDecisionArgs). The push card passes no opts at all.
  const decideAdoDirect = async (id: string, approve: boolean, opts: [] | [DecisionOptions]): Promise<void> => {
    try {
      if (approve) await approvalsApi.approve(id, "approved", ...opts);
      else await approvalsApi.deny(id, "denied", ...opts);
      toast.success(approve ? APPROVALS.TOAST_APPROVED : APPROVALS.TOAST_DENIED);
      void load(false);
    } catch (err) {
      toast.error(approve ? APPROVALS.TOAST_APPROVE_FAILED : APPROVALS.TOAST_DENY_FAILED, { description: getErrorMessage(err) });
    }
  };

  // decidePushDirect — the Approvals tab's own push_content decide path
  // (#181, review finding 3). Same reason decideAdoDirect bypasses
  // ReasonDialog/submitDecision: the card's own control carries no reason
  // field — but unlike ADO, NO opts at all (decide's rule 4 refuses a
  // decision_scope on this kind).
  const decidePushDirect = (id: string, approve: boolean): Promise<void> => decideAdoDirect(id, approve, []);

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
            viewerBlocked={viewerBlockedOnDecision(pending, securityOperator, run.created_by === principal)}
            sandboxHeld={pending.some(isHeld)}
            // F13 — split, so the AWS-named chip never fires for an Azure
            // DevOps consent row (isAdoConsentRequest is also credential_reauth).
            awaitingReauth={pending.some((p) => p.kind === "credential_reauth" && !isAdoConsentRequest(p))}
            awaitingAdoConsent={pending.some(isAdoConsentRequest)}
            onGoApprovals={() => setTab("approvals")}
            onCopyLink={copyLink}
            onKill={kill}
            canKillAgain={killAgain}
            onClone={view === "user" ? () => void onClone() : undefined}
            onRename={view === "user" ? rename : undefined}
          />

          <LaunchWarningsNote />
          {/* RL-15 (#580, #1197 L5): one lifetime banner (lost/ended/paused/
              ending-soon — mutually exclusive server facts) plus the
              persistent Ends/wait row, both self-contained per the
              LaunchWarningsNote pattern above. */}
          <RunLifetimeWithContainment run={run} pending={pending} onChanged={() => load(false)} />
          <RunEndsRow run={run} onChanged={() => load(false)} />

          <RunDetailCommandBar
            tabs={
              <TabsList className="h-7 bg-transparent p-0">
                <TabsTrigger value="overview" className={RUN_TAB}>
                  <LayoutDashboard className="size-3.5" /> Overview
                </TabsTrigger>
                <TabsTrigger value="approvals" className={RUN_TAB}>
                  <ShieldCheck className="size-3.5" /> Approvals
                  {pending.length > 0 && (
                    <span className="rounded-full bg-warning-subtle px-1.5 text-meta font-semibold text-warning">
                      {pending.length}
                    </span>
                  )}
                </TabsTrigger>
                <TabsTrigger value="policy" className={RUN_TAB}>
                  <FileText className="size-3.5" /> {POLICY_TAB.tab}
                </TabsTrigger>
                <TabsTrigger value="audit" className={RUN_TAB}>
                  <ScrollText className="size-3.5" /> Audit
                </TabsTrigger>
                <TabsTrigger value="recording" className={RUN_TAB}>
                  <SquareTerminal className="size-3.5" /> Recording
                </TabsTrigger>
                <TabsTrigger value="output" className={RUN_TAB}>
                  <Logs className="size-3.5" /> {RUN_OUTPUT.tab}
                </TabsTrigger>
              </TabsList>
            }
          />

          {/* Overview is the cockpit: it fills, and the CANVAS owns the only
              scroll inside it (run-detail/canvas.tsx) — <main> still never
              scrolls, which e2e asserts. */}
          <TabsContent value="overview" className="mt-0 flex min-h-0 flex-1 flex-col">
            <Cockpit
              run={run} view={view}
              terminal={terminal}
              grants={grants}
              egress={egress}
              audit={endingEvents}
              auditGap={auditGap}
              held={held}
              outcomeReady={outcomeReady}
              pending={pending}
              recording={recording}
              recState={recState}
              recordingDisabled={recordingDisabled}
              onGoAudit={() => setTab("audit")}
              onGoPolicy={() => setTab("policy")}
              onGoRecording={() => setTab("recording")}
              onGoApprovals={() => setTab("approvals")}
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

          <TabsContent value="policy" className="scroll-thin mt-0 min-h-0 flex-1 overflow-y-auto p-4">
            <PolicyTab run={run} />
          </TabsContent>

          <TabsContent value="audit" className="scroll-thin mt-0 min-h-0 flex-1 overflow-y-auto p-4">
            <AuditTab events={audit} runId={run.id} policy={run.policy} onMakePolicy={() => setProfileRunId(run.id)} />
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

          <TabsContent value="output" className="scroll-thin mt-0 min-h-0 flex-1 overflow-y-auto p-4">
            <OutputTab
              runId={run.id}
              live={!terminal}
              state={run.state}
              endedAt={run.ended_at}
              onGoRecording={() => setTab("recording")}
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

// Every human attach session is recorded under a COMPOSITE cast key the
// console never asked for; the index is the action_prefix="session.recording"
// fetch above (not the capped general trail), keyed on the event's TARGET.
// The filter is an EXACT match on the CANONICAL name (canonicalAuditAction),
// not the prefix — a plain startsWith would also admit session.recording.other.
function attachSessions(audit: AuditEvent[]): AuditEvent[] {
  const seen = new Set<string>(); // dedup, belt-and-braces
  return audit.filter((e) => canonicalAuditAction(e.action) === "session.recording.write" && e.outcome === "success" && e.target && !seen.has(e.id) && seen.add(e.id));
}

// Audit tab (this run's events)
function AuditTab({
  events,
  runId,
  policy,
  onMakePolicy,
}: {
  events: AuditEvent[];
  runId: string;
  /** The run's own policy (GET /runs/{id}), never /me: an admin viewing someone
   *  else's run must see that run's owner, not their own. */
  policy?: RunDetail["policy"];
  onMakePolicy: () => void;
}) {
  const securityOperator = useSecurityOperator();
  return (
    <div className="max-w-4xl">
      <div className="mb-3 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
        <ScrollText className="size-3.5" />
        Append-only · {events.length} event{events.length === 1 ? "" : "s"} for this run
        {/* W25-W25.2-3: carry the run, so the full feed opens scoped to it.
            M-1b: the full-page Audit screen is Admin view only now
            (/admin/audit), so the link renders only for the tier that screen
            serves; a user's own events are already inline above. */}
        {securityOperator && (
          <Link
            to={`/admin/audit?run_id=${runId}`}
            className="ml-1 inline-flex items-center gap-1 text-primary hover:underline"
          >
            open full Audit <ArrowRight className="size-3" />
          </Link>
        )}
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
              // One line from sm up; below sm the target drops to a second line of its own (#2917).
              <div key={e.id} data-testid="run-audit-row" className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5 sm:flex-nowrap">
                <span
                  data-testid="run-audit-time"
                  className="w-[84px] shrink-0 whitespace-nowrap font-mono text-meta text-muted-foreground"
                  title={absoluteTime(e.time)}
                >
                  {clockTime(e.time)}
                </span>
                {/* A fixed slot, so the action and target columns start at one x for every actor. */}
                <span className="inline-flex w-[88px] shrink-0">
                  <ActorTypeChip type={e.actor_type} />
                </span>
                <span
                  className="min-w-0 grow basis-[120px] truncate font-mono text-xs text-muted-foreground sm:w-[190px] sm:grow-0 sm:basis-auto sm:shrink-0"
                  title={e.action}
                >
                  {e.action}
                </span>
                {/* A tool call the policy's own tool_rules answered rides an
                    egress.allow/deny row whose target is the CONTROL PLANE —
                    so the row says who decided, as the Audit screen does. */}
                {toolRuleDecision(e) ? (
                  <AuditDecision event={e} className="flex min-w-0 grow basis-full items-center gap-2 text-xs sm:basis-0" />
                ) : (
                  <span data-testid="run-audit-target" className="flex min-w-0 grow basis-full items-center gap-2 sm:basis-0">
                    {e.target === ERASED_VALUE ? (
                      <span className="min-w-0 flex-1 truncate text-xs text-muted-foreground" title={AUDIT.ERASED_HINT}>
                        {AUDIT.ERASED}
                      </span>
                    ) : (
                      <span className="min-w-0 flex-1 truncate text-xs text-foreground" title={e.target}>
                        {e.target || "—"}
                      </span>
                    )}
                    <RuleSourceChip event={e} />
                    {isAttributedRefusal(e) && <PolicyRemedy policy={policy} className="shrink-0" />}
                    <ErasedFields event={e} className="shrink-0 text-xs" />
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

