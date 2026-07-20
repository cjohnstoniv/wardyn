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
  ChevronRight,
  Copy,
  Fingerprint,
  Globe,
  KeyRound,
  LayoutDashboard,
  Link as LinkIcon,
  Loader2,
  ScrollText,
  ShieldCheck,
  Skull,
  SquareTerminal,
  TerminalSquare,
} from "lucide-react";
import { toast } from "sonner";
import type {
  AgentRun,
  ApprovalRequest,
  AuditEvent,
  CredentialGrant,
  EgressDecision,
  Recording,
} from "../../lib/types";
import { isTerminalRunState } from "../../lib/types";
import { runs as runsApi } from "../../lib/api/runs";
import { approvals as approvalsApi } from "../../lib/api/approvals";
import { audit as auditApi, egressFromAudit } from "../../lib/api/audit";
import { recordings as recordingsApi } from "../../lib/api/recordings";
import { usePoll } from "../../lib/use-poll";
import { useCopyToClipboard } from "../../lib/use-copy-to-clipboard";
import { absoluteTime, getErrorMessage, relativeTime } from "../../lib/format";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "../ui/alert-dialog";
import { Button } from "../ui/button";
import { Label } from "../ui/label";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "../ui/tabs";
import {
  ActorTypeChip,
  AgentBadge,
  ApprovalKindChip,
  ApprovalStateBadge,
  Chip,
  ConfinementChip,
  EgressDecisionChip,
} from "../wardyn/primitives";
import { BarrierStrengthStrip } from "../wardyn/barrier-strength-strip";
import { RunStatusBadge } from "../wardyn/run-status-badge";
import { JsonBlock, Mono } from "../wardyn/code-block";
import { EmptyState, ErrorState, TableSkeleton } from "../wardyn/states";
import { TerminalPlayer } from "../wardyn/terminal-player";
import { AttachTerminal } from "../attach-terminal";
import { LiveApprovals } from "../wardyn/live-approvals";
import { ReasonDialog } from "../wardyn/reason-dialog";
import { cn } from "../ui/utils";

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
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [tab, setTab] = React.useState<Tab>("overview");

  // Recording is fetched lazily the first time the Recording tab opens.
  const [recording, setRecording] = React.useState<Recording | null>(null);
  const [recState, setRecState] = React.useState<"idle" | "loading" | "error" | "ready">("idle");

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
      ])
        .then(([r, g, allApprovals, a]) => {
          setRun(r ?? null);
          setGrants(g);
          setEgress(egressFromAudit(a));
          setApprovals(allApprovals.filter((x) => x.run_id === id));
          setAudit(a);
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
    load(true);
  }, [id, load]);

  const terminal = run ? isTerminalRunState(run.state) : true;
  usePoll(() => load(false), DETAIL_POLL_MS, terminal);

  // Lazy recording load on first Recording-tab open.
  React.useEffect(() => {
    if (tab !== "recording" || !id || recState !== "idle") return;
    setRecState("loading");
    recordingsApi
      .getRecording(id)
      .then((rec) => {
        setRecording(rec ?? null);
        setRecState("ready");
      })
      .catch(() => setRecState("error"));
  }, [tab, id, recState]);

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

  const submitDecision = async (reason: string): Promise<boolean> => {
    if (!decide) return false;
    try {
      if (decide.action === "approve") await approvalsApi.approve(decide.id, reason);
      else await approvalsApi.deny(decide.id, reason);
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
  const shortId = id.replace(/^run_/, "");

  return (
    <div className="mx-auto max-w-[1200px] px-6 py-5">
      {/* Breadcrumb + copy-link */}
      <div className="mb-4 flex items-center gap-2 text-sm">
        <Link to="/runs" className="text-muted-foreground hover:text-foreground hover:underline">
          Runs
        </Link>
        <ChevronRight className="size-3.5 text-muted-foreground" aria-hidden />
        <Mono className="text-foreground" title={id}>
          {shortId}
        </Mono>
        <Button variant="outline" size="sm" className="ml-1 h-6 gap-1 px-2 text-[0.7188rem]" onClick={copyLink}>
          {copied ? <Check className="size-3 text-success" /> : <LinkIcon className="size-3" />}
          {copied ? "Copied" : "Copy link"}
        </Button>
      </div>

      {status === "loading" ? (
        <div className="space-y-4">
          <div className="rounded-xl border border-border bg-card p-5">
            <div className="h-6 w-72 animate-pulse rounded bg-muted" />
            <div className="mt-3 h-4 w-96 animate-pulse rounded bg-muted" />
          </div>
          <div className="rounded-xl border border-border bg-card">
            <TableSkeleton rows={6} cols={3} />
          </div>
        </div>
      ) : status === "error" ? (
        <div className="rounded-xl border border-border bg-card">
          <ErrorState onRetry={() => load(true)} />
        </div>
      ) : !run ? (
        <div className="rounded-xl border border-border bg-card">
          <EmptyState
            icon={ScrollText}
            title="Run not found"
            description="This run may have been archived or deleted, or the link is stale."
            action={
              <Button variant="outline" onClick={() => navigate("/runs")}>
                Back to Runs
              </Button>
            }
          />
        </div>
      ) : (
        <>
          <SummaryHeader run={run} terminal={terminal} onKill={kill} />

          <Tabs value={tab} onValueChange={(v) => setTab(v as Tab)} className="mt-5">
            <TabsList className="mb-5">
              <TabsTrigger value="overview" className="gap-1.5">
                <LayoutDashboard className="size-3.5" /> Overview
              </TabsTrigger>
              <TabsTrigger value="approvals" className="gap-1.5">
                <ShieldCheck className="size-3.5" /> Approvals
                {pendingCount(approvals) > 0 && (
                  <span className="rounded-full bg-warning-subtle px-1.5 text-[0.6563rem] font-semibold text-warning">
                    {pendingCount(approvals)}
                  </span>
                )}
              </TabsTrigger>
              <TabsTrigger value="audit" className="gap-1.5">
                <ScrollText className="size-3.5" /> Audit
              </TabsTrigger>
              <TabsTrigger value="recording" className="gap-1.5">
                <SquareTerminal className="size-3.5" /> Recording
              </TabsTrigger>
            </TabsList>

            <TabsContent value="overview" className="mt-0">
              <OverviewTab
                run={run}
                grants={grants}
                egress={egress}
                audit={audit}
                approvals={approvals}
                onGoApprovals={() => setTab("approvals")}
                onGoAudit={() => setTab("audit")}
                onGoRecording={() => setTab("recording")}
              />
            </TabsContent>

            <TabsContent value="approvals" className="mt-0">
              <ApprovalsTab
                approvals={approvals}
                onDecide={(approvalId, action, kind) => setDecide({ id: approvalId, action, kind })}
              />
            </TabsContent>

            <TabsContent value="audit" className="mt-0">
              <AuditTab events={audit} />
            </TabsContent>

            <TabsContent value="recording" className="mt-0">
              <RecordingTab
                state={recState}
                recording={recording}
                onRetry={() => setRecState("idle")}
              />
            </TabsContent>
          </Tabs>
        </>
      )}

      <ReasonDialog
        prompt={decide}
        onClose={() => setDecide(null)}
        onSubmit={submitDecision}
      />
    </div>
  );
}

function pendingCount(approvals: ApprovalRequest[]): number {
  return approvals.filter((a) => a.state === "PENDING").length;
}

// ---------------------------------------------------------------------------
// Summary header
// ---------------------------------------------------------------------------
function SummaryHeader({
  run,
  terminal,
  onKill,
}: {
  run: AgentRun;
  terminal: boolean;
  onKill: () => void;
}) {
  return (
    <div className="rounded-xl border border-border bg-card p-5">
      <div className="flex flex-wrap items-start gap-4">
        <AgentBadge agent={run.agent} withLabel={false} />
        <div className="min-w-[260px] flex-1">
          <h1 className="text-xl font-semibold leading-tight text-foreground">{run.task || "—"}</h1>
          <div className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground">
            <AgentBadge agent={run.agent} />
            <span>·</span>
            <span className="font-mono">{run.repo}</span>
            <span>·</span>
            <span title={run.created_at}>started {relativeTime(run.created_at)} by {run.created_by}</span>
          </div>
          <div className="mt-3 flex flex-wrap items-center gap-2">
            <RunStatusBadge state={run.state} />
            <ConfinementChip value={run.confinement_class} />
            <BarrierStrengthStrip tier={run.confinement_class} />
            {run.interactive && (
              <Chip tone="info" className="gap-1">
                <TerminalSquare className="size-3" />
                {run.state === "RUNNING" ? "Interactive — attachable" : "Interactive"}
              </Chip>
            )}
          </div>
        </div>
        <AlertDialog>
          <AlertDialogTrigger asChild>
            <Button variant="outline" size="sm" className="text-danger hover:text-danger" disabled={terminal}>
              <Skull className="size-4" /> Kill
            </Button>
          </AlertDialogTrigger>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>Kill {run.id}?</AlertDialogTitle>
              <AlertDialogDescription>
                This terminates the agent run, tears down the sandbox, and revokes any brokered
                credentials. Enforcement stop — it is recorded in the audit trail. This cannot be undone.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>Cancel</AlertDialogCancel>
              <AlertDialogAction onClick={onKill} className="bg-destructive text-white hover:bg-destructive/90">
                Kill run
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Overview
// ---------------------------------------------------------------------------
function OverviewTab({
  run,
  grants,
  egress,
  audit,
  approvals,
  onGoApprovals,
  onGoAudit,
  onGoRecording,
}: {
  run: AgentRun;
  grants: CredentialGrant[];
  egress: EgressDecision[];
  audit: AuditEvent[];
  approvals: ApprovalRequest[];
  onGoApprovals: () => void;
  onGoAudit: () => void;
  onGoRecording: () => void;
}) {
  const pending = approvals.filter((a) => a.state === "PENDING");
  const attachable = !!run.interactive && run.state === "RUNNING";

  return (
    <div className="grid grid-cols-1 items-start gap-4 lg:grid-cols-[minmax(0,1.9fr)_minmax(0,1fr)]">
      {/* main column */}
      <div className="flex min-w-0 flex-col gap-4">
        {pending.length > 0 && (
          <div className="rounded-xl border border-warning/35 bg-warning-subtle/60 p-4">
            <div className="flex items-start gap-3">
              <ShieldCheck className="mt-0.5 size-4 shrink-0 text-warning" />
              <div className="min-w-0 flex-1">
                <div className="text-sm font-semibold text-foreground">
                  Waiting for your confirmation
                </div>
                <p className="mt-1 text-sm leading-relaxed text-muted-foreground">
                  This run has {pending.length} pending approval{pending.length === 1 ? "" : "s"}. Review the
                  exact requested scope before you decide — nothing is minted until you approve.
                </p>
                <Button size="sm" className="mt-3" onClick={onGoApprovals}>
                  Review approvals <ArrowRight className="size-3.5" />
                </Button>
              </div>
            </div>
          </div>
        )}

        <SectionCard
          title="Run timeline"
          right={<span className="text-xs text-muted-foreground">{audit.length} events</span>}
        >
          {audit.length === 0 ? (
            <EmptyMini text="No audit events recorded for this run yet." />
          ) : (
            <Timeline events={audit.slice(-12)} />
          )}
          {audit.length > 12 && (
            <button
              onClick={onGoAudit}
              className="mt-3 text-[0.7813rem] font-medium text-primary hover:underline"
            >
              View full trail in Audit →
            </button>
          )}
        </SectionCard>

        <SectionCard
          title="Live terminal"
          right={
            <button onClick={onGoRecording} className="text-[0.7813rem] font-medium text-primary hover:underline">
              Open full recording →
            </button>
          }
        >
          {attachable ? (
            <AttachTerminal runId={run.id} />
          ) : (
            <EmptyMini
              text={
                run.state === "RUNNING"
                  ? "This run is autonomous — the agent drives it. Watch the captured session under Recording."
                  : "The run isn't live. Replay the captured terminal session under Recording."
              }
            />
          )}
          {/* Live egress approvals, co-located with the terminal: a held
              (wait_for_review) request pauses the sandbox until you decide it
              right here — no need to leave for the Approvals tab. Interactive OR
              autonomous, as long as the run is live. */}
          {run.state === "RUNNING" && (
            <div className="mt-3">
              <LiveApprovals runId={run.id} />
            </div>
          )}
        </SectionCard>
      </div>

      {/* side column */}
      <div className="flex min-w-0 flex-col gap-4">
        <SectionCard title="Identity" Icon={Fingerprint}>
          <dl className="grid grid-cols-[auto_1fr] gap-x-3.5 gap-y-2 text-[0.7813rem]">
            <KV label="Run ID">
              <Mono className="text-foreground">{run.id}</Mono>
            </KV>
            <KV label="Identity">
              <CopyValue value={run.spiffe_id} />
            </KV>
            <KV label="Repository">
              <span className="font-mono text-foreground">{run.repo}</span>
            </KV>
            {run.workspace_path && (
              <KV label="Workspace">
                <span className="break-all font-mono text-foreground">{run.workspace_path}</span>
              </KV>
            )}
            <KV label="Runner">
              <span className="font-mono text-foreground">{run.runner_target}</span>
            </KV>
            {run.sandbox_ref && (
              <KV label="Sandbox">
                <span className="break-all font-mono text-foreground">{run.sandbox_ref}</span>
              </KV>
            )}
            {run.image && (
              <KV label="Image">
                <span className="break-all font-mono text-foreground">{run.image}</span>
              </KV>
            )}
            {run.policy_id && (
              <KV label="Policy">
                <span className="font-mono text-foreground">{run.policy_id}</span>
              </KV>
            )}
            <KV label="Created by">
              <span className="text-foreground">{run.created_by}</span>
            </KV>
            <KV label="Started">
              <span className="text-foreground">{absoluteTime(run.created_at)}</span>
            </KV>
          </dl>
        </SectionCard>

        <GrantsCard grants={grants} audit={audit} />

        <SectionCard
          title="Egress"
          Icon={Globe}
          right={<span className="text-xs text-muted-foreground">{egress.length}</span>}
        >
          {egress.length === 0 ? (
            <EmptyMini text="No outbound connections recorded yet." />
          ) : (
            <div className="flex flex-col gap-2">
              {egress.slice(-6).map((e) => (
                <div key={e.id} className="flex items-center gap-2 text-[0.7813rem]">
                  <EgressDecisionChip decision={e.decision} />
                  <span className="min-w-0 truncate font-mono text-muted-foreground" title={e.domain}>
                    {e.domain}
                  </span>
                  <span className="ml-auto whitespace-nowrap font-mono text-[0.6875rem] text-muted-foreground">
                    {relativeTime(e.time)}
                  </span>
                </div>
              ))}
            </div>
          )}
          <button
            onClick={onGoAudit}
            className="mt-3 text-[0.7813rem] font-medium text-primary hover:underline"
          >
            Full history in Audit →
          </button>
        </SectionCard>
      </div>
    </div>
  );
}

// Credential grants — HONEST (audit finding #15). Grants are ELIGIBILITY
// records (what the run MAY request), NOT live/active credentials. Active
// credentials are driven ONLY from `credential.mint` audit events.
function GrantsCard({ grants, audit }: { grants: CredentialGrant[]; audit: AuditEvent[] }) {
  // Only SUCCESSFUL mints are real issued credentials — the broker also audits
  // DENIED mint attempts under credential.mint (outcome!="success"); showing those
  // as "brokered" would claim a credential that was never issued (finding #15).
  const minted = audit.filter((e) => e.action === "credential.mint" && e.outcome === "success");
  return (
    <SectionCard title="Credential grants" Icon={KeyRound}>
      <p className="mb-3 text-[0.75rem] leading-relaxed text-muted-foreground">
        Eligibility — what this run may request. When you approve, the broker mints a short-lived,
        scoped token; the agent never sees your real keys.
      </p>

      {grants.length === 0 ? (
        <EmptyMini text="No credential grants are configured for this run." />
      ) : (
        <div className="flex flex-col gap-2">
          {grants.map((g) => (
            <div key={g.id} className="flex items-start gap-2.5 rounded-lg border border-border p-2.5">
              <KeyRound className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
              <div className="min-w-0 flex-1">
                <div className="break-all font-mono text-[0.7813rem] text-foreground">{g.scope}</div>
                <div className="mt-1">
                  <Chip tone="neutral" className="text-[0.6875rem]">
                    eligible to mint
                  </Chip>
                </div>
              </div>
            </div>
          ))}
        </div>
      )}

      {minted.length > 0 && (
        <div className="mt-3">
          <div className="mb-1.5 text-[0.6875rem] font-semibold uppercase tracking-wide text-muted-foreground">
            Minted this run
          </div>
          <div className="flex flex-col gap-1.5">
            {minted.map((e) => (
              <div key={e.id} className="flex items-center gap-2 text-[0.75rem]">
                <Chip tone="info" className="text-[0.6875rem]">brokered</Chip>
                <span className="min-w-0 truncate font-mono text-muted-foreground" title={e.target}>
                  {e.target || "credential"}
                </span>
                <span className="ml-auto whitespace-nowrap font-mono text-[0.6875rem] text-muted-foreground">
                  {relativeTime(e.time)}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}

      {grants.length > 0 && (
        <details className="mt-3">
          <summary className="inline-flex cursor-pointer items-center gap-1.5 text-[0.75rem] text-muted-foreground hover:text-foreground">
            View raw JSON
          </summary>
          <JsonBlock value={grants} className="mt-2" />
        </details>
      )}
    </SectionCard>
  );
}

function Timeline({ events }: { events: AuditEvent[] }) {
  return (
    <div className="flex flex-col">
      {events.map((e, i) => {
        const last = i === events.length - 1;
        const tint = eventTint(e);
        return (
          <div key={e.id} className="flex gap-3">
            <div className="flex w-4 shrink-0 flex-col items-center">
              <span className={cn("mt-1 size-2 rounded-full", tint)} />
              {!last && <span className="my-1 w-px flex-1 bg-border" />}
            </div>
            <div className={cn("min-w-0 flex-1", last ? "pb-0" : "pb-4")}>
              <div className="flex items-baseline gap-2">
                <span className="min-w-0 truncate font-mono text-[0.7813rem] text-foreground" title={e.action}>
                  {e.action}
                </span>
                <ActorTypeChip type={e.actor_type} />
                <span className="ml-auto whitespace-nowrap font-mono text-[0.6875rem] text-muted-foreground">
                  {new Date(e.time).toLocaleTimeString(undefined, {
                    hour: "2-digit",
                    minute: "2-digit",
                    second: "2-digit",
                  })}
                </span>
              </div>
              {e.target && (
                <div className="mt-0.5 break-words text-[0.75rem] leading-relaxed text-muted-foreground">
                  {e.target}
                </div>
              )}
            </div>
          </div>
        );
      })}
    </div>
  );
}

function eventTint(e: AuditEvent): string {
  if (e.action === "run.kill" || e.action.startsWith("egress.deny")) return "bg-danger";
  if (e.action.startsWith("approval.")) return "bg-warning";
  if (e.outcome === "denied") return "bg-warning";
  if (e.outcome === "failure") return "bg-danger";
  return "bg-muted-foreground";
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
                {a.reason && <> · {a.reason}</>}
              </div>
            )}
            {pending && (
              <div className="mt-3 flex items-center justify-end gap-2 border-t border-border pt-3">
                <Button variant="outline" size="sm" onClick={() => onDecide(a.id, "deny", a.kind)}>
                  Deny
                </Button>
                <Button size="sm" variant="info" onClick={() => onDecide(a.id, "approve", a.kind)}>
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
function AuditTab({ events }: { events: AuditEvent[] }) {
  return (
    <div className="max-w-4xl">
      <div className="mb-3 flex items-center gap-2 text-xs text-muted-foreground">
        <ScrollText className="size-3.5" />
        Append-only · {events.length} event{events.length === 1 ? "" : "s"} for this run
        <Link to="/audit" className="ml-1 inline-flex items-center gap-1 text-primary hover:underline">
          open full Audit <ArrowRight className="size-3" />
        </Link>
      </div>
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
                  {new Date(e.time).toLocaleTimeString(undefined, {
                    hour: "2-digit",
                    minute: "2-digit",
                    second: "2-digit",
                  })}
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
  onRetry,
}: {
  state: "idle" | "loading" | "error" | "ready";
  recording: Recording | null;
  onRetry: () => void;
}) {
  if (state === "loading" || state === "idle") {
    return (
      <div className="flex h-[360px] items-center justify-center rounded-xl border border-border bg-card">
        <Loader2 className="size-5 animate-spin text-muted-foreground" />
      </div>
    );
  }
  if (state === "error") {
    return (
      <div className="rounded-xl border border-border bg-card">
        <ErrorState message="Couldn't load this run's recording." onRetry={onRetry} />
      </div>
    );
  }
  if (!recording) {
    return (
      <div className="rounded-xl border border-border bg-card">
        <EmptyState
          icon={SquareTerminal}
          title="No recording available"
          description="This run has no captured terminal session. A recording is produced once an agent process runs in the sandbox."
        />
      </div>
    );
  }
  return (
    <div className="max-w-4xl">
      <TerminalPlayer recording={recording} />
      <div className="mt-2 text-xs text-muted-foreground">
        Recorded when the run's runner supports session capture ·{" "}
        <Link to="/recordings" className="text-primary hover:underline">
          Recordings library
        </Link>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Small shared bits
// ---------------------------------------------------------------------------
function SectionCard({
  title,
  Icon,
  right,
  children,
}: {
  title: string;
  Icon?: React.ElementType;
  right?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <section className="rounded-xl border border-border bg-card p-4">
      <div className="mb-3 flex items-center gap-2">
        {Icon && <Icon className="size-4 text-muted-foreground" />}
        <h2 className="text-[0.6875rem] font-semibold uppercase tracking-wider text-muted-foreground">{title}</h2>
        {right && <div className="ml-auto">{right}</div>}
      </div>
      {children}
    </section>
  );
}

function KV({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0 text-right">{children}</dd>
    </>
  );
}

function CopyValue({ value }: { value: string }) {
  const { copied, copy } = useCopyToClipboard(1400);
  return (
    <span className="flex min-w-0 items-center justify-end gap-1.5">
      <span className="truncate font-mono text-foreground" title={value}>
        {value}
      </span>
      <button
        onClick={() => copy(value)}
        aria-label="Copy"
        className="shrink-0 text-muted-foreground hover:text-foreground"
      >
        {copied ? <Check className="size-3 text-success" /> : <Copy className="size-3" />}
      </button>
    </span>
  );
}

function EmptyMini({ text }: { text: string }) {
  return (
    <div className="rounded-lg border border-dashed border-border px-4 py-6 text-center text-sm text-muted-foreground">
      {text}
    </div>
  );
}
