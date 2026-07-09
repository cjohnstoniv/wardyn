/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Link } from "react-router-dom";
import {
  ScrollText,
  Search,
  ArrowRight,
  AlertTriangle,
  Globe,
  TerminalSquare,
  KeyRound,
  ShieldCheck,
  Activity,
  Skull,
  CircleCheck,
  CircleX,
  Plus,
} from "lucide-react";
import type { AuditEvent, ActorType, AgentRun } from "../../lib/types";
import { api } from "../../lib/api";
import { absoluteTime } from "../../lib/format";
import { usePoll } from "../../lib/use-poll";
import { Input } from "../ui/input";
import { Button } from "../ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../ui/select";
import {
  ActorTypeChip,
  AgentBadge,
  Chip,
  ConfinementChip,
  OutcomeBadge,
  SectionLabel,
} from "../wardyn/primitives";
import { Mono } from "../wardyn/code-block";
import { EmptyState, ErrorState, TableSkeleton } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { NewRunDialog } from "./new-run/new-run-dialog";
import { cn } from "../ui/utils";

// The backend caps an audit page at this many events. When a response comes back
// at exactly the cap we can't tell whether more exist, so we surface a "showing
// first N (truncated)" indicator. Keep this in sync with the server's page cap.
const AUDIT_PAGE_CAP = 500;

// M20 fix: a run_id-filtered query hits a DIFFERENT, higher server cap
// (QueryAuditEvents's default limit=1000, vs QueryRecentAuditEvents's 500 for
// the unfiltered/global view) — a long-running/chatty run's OWN trail can still
// truncate at this cap. Keep in sync with the server's per-run page cap.
const RUN_AUDIT_PAGE_CAP = 1000;

// Audit is append-only, so live-tailing is meaningful (unlike a poll on mutable
// state). Kept modest — this is a background refresh, not a chat stream.
const AUDIT_POLL_MS = 5000;

// ------------------------------------------------------------------
// Event-kind bucketing. AuditEvent.action is an open dotted-verb string the
// backend owns (internal/types/types.go); these are the REAL prefixes/values
// wardynd emits today:
//   egress.allow / egress.deny / egress.pending, llm.scan.<action>
//   kernel.process.exec / kernel.network.connect / kernel.file.write / ...
//   credential.mint / credential.revoke, identity.mint / identity.revoke,
//   secret.read / secret.write / secret.delete
//   approval.decide / approval.expire
//   run.kill
//   everything else — run.create/build/dispatch/complete/..., session.*,
//   policy.*, recording.upload, run.compose* — is run/session/policy lifecycle.
// Bucketing is prefix-based so an action the table below doesn't know about yet
// degrades into "lifecycle" (the catch-all) instead of vanishing from a facet.
// ------------------------------------------------------------------
type EventKind = "egress" | "tool" | "credentials" | "approvals" | "lifecycle" | "enforcement";

const KIND_META: Record<EventKind, { label: string; Icon: React.ElementType }> = {
  egress: { label: "Egress", Icon: Globe },
  tool: { label: "Tool calls", Icon: TerminalSquare },
  credentials: { label: "Credentials", Icon: KeyRound },
  approvals: { label: "Approvals", Icon: ShieldCheck },
  lifecycle: { label: "Lifecycle", Icon: Activity },
  enforcement: { label: "Enforcement", Icon: Skull },
};

// Display order for the Event facet — matches how an operator scans severity:
// network -> execution -> credentials -> human review -> everything else -> kill.
const KIND_ORDER: EventKind[] = ["egress", "tool", "credentials", "approvals", "lifecycle", "enforcement"];

function eventKind(action: string): EventKind {
  if (action === "run.kill") return "enforcement";
  if (action.startsWith("egress.") || action.startsWith("llm.scan.")) return "egress";
  if (action.startsWith("kernel.") || action === "run.exec") return "tool";
  if (action.startsWith("credential.") || action.startsWith("identity.") || action.startsWith("secret.")) {
    return "credentials";
  }
  if (action.startsWith("approval.")) return "approvals";
  return "lifecycle";
}

// Short, human verbs for the actions we know about. Anything not in this table
// falls back to the raw dotted action — a guess at prose for a backend action we
// don't recognize would be dishonest; the raw verb is always true.
const ACTION_VERB: Record<string, string> = {
  "run.create": "created the run",
  "run.build": "built the sandbox image",
  "run.dispatch": "dispatched the run",
  "run.interactive": "started the run idle for attach",
  "run.exec": "executed the agent task",
  "run.complete": "completed",
  "run.kill": "killed the run",
  "run.revoke": "revoked the run's identity",
  "run.autostop": "auto-stopped the idle run",
  "run.reconcile": "reconciled run state",
  "run.workspace.collision": "detected a workspace directory collision",
  "run.record.synthesize": "synthesized a least-privilege profile from the recording",
  "run.compose": "produced a run proposal",
  "run.compose.clarify": "asked a clarifying question",
  "run.compose.assist": "answered a composer question",
  "run.compose.client": "recorded a composer funnel event",
  "credential.mint": "minted a credential",
  "credential.revoke": "revoked a credential",
  "identity.mint": "minted a workload identity",
  "identity.revoke": "revoked a workload identity",
  "secret.read": "read a secret into the run",
  "secret.write": "stored a secret",
  "secret.delete": "deleted a secret",
  "approval.decide": "decided an approval request",
  "approval.expire": "an approval request expired",
  "policy.create": "created a policy",
  "policy.update": "updated a policy",
  "policy.delete": "deleted a policy",
  "policy.inline": "applied an inline policy",
  "session.attach": "attached to the run's terminal",
  "session.detach": "detached from the run's terminal",
  "session.recording": "recorded the terminal session",
  "recording.upload": "uploaded the run recording",
  "kernel.process.exec": "observed a process exec",
  "kernel.network.connect": "observed a network connect",
  "kernel.file.write": "observed a write to a sensitive path",
  "kernel.sensor.heartbeat": "sensor heartbeat",
  "kernel.sensor.blind": "kernel sensor blind — no ground truth for this run",
};

function capitalize(s: string): string {
  return s.length ? s[0].toUpperCase() + s.slice(1) : s;
}

// Human description built ONLY from real fields (action + target). egress.* and
// llm.scan.* are dynamic families (the suffix is the decision/scan outcome), so
// they're handled directly rather than enumerated in ACTION_VERB.
function describeEvent(e: AuditEvent): string {
  if (e.action.startsWith("egress.")) {
    const decision = e.action.slice("egress.".length);
    const verb =
      decision === "allow" ? "Allowed egress to" : decision === "deny" ? "Denied egress to" : "Deferred egress to";
    return `${verb} ${e.target || "an unknown host"}`;
  }
  if (e.action.startsWith("llm.scan.")) {
    return `LLM content scan (${e.action.slice("llm.scan.".length)}) for ${e.target || "an unknown host"}`;
  }
  const verb = ACTION_VERB[e.action];
  if (verb) return e.target ? `${capitalize(verb)} — ${e.target}` : capitalize(verb);
  return e.target ? `${e.action} — ${e.target}` : e.action;
}

// ------------------------------------------------------------------
// Day grouping. Groups are created in the order their day is first seen while
// walking the (already server-ordered) event list — never re-sorted by
// wall-clock time. That preserves append (seq) order both within a day and
// across days, whichever direction the current query returned (newest-first
// for the global window, oldest-first for a per-run trail).
// ------------------------------------------------------------------
interface DayGroup {
  key: number;
  label: string;
  events: AuditEvent[];
}

function dayStart(iso: string): number {
  const d = new Date(iso);
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
}

function dayLabel(ts: number): string {
  const now = new Date();
  const todayStart = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const oneDayMs = 86_400_000;
  if (ts === todayStart) return "Today";
  if (ts === todayStart - oneDayMs) return "Yesterday";
  return new Date(ts).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "2-digit" });
}

function groupByDay(events: AuditEvent[]): DayGroup[] {
  const groups: DayGroup[] = [];
  const index = new Map<number, number>();
  for (const e of events) {
    const key = dayStart(e.time);
    let idx = index.get(key);
    if (idx === undefined) {
      idx = groups.length;
      index.set(key, idx);
      groups.push({ key, label: dayLabel(key), events: [] });
    }
    groups[idx].events.push(e);
  }
  return groups;
}

export function AuditScreen() {
  const [events, setEvents] = React.useState<AuditEvent[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [truncated, setTruncated] = React.useState(false);
  const [query, setQuery] = React.useState("");
  const [runFilter, setRunFilter] = React.useState("");
  const [kindFilter, setKindFilter] = React.useState<EventKind | "all">("all");
  const [actorFilter, setActorFilter] = React.useState<ActorType | "all">("all");
  const [newRunOpen, setNewRunOpen] = React.useState(false);

  // Drill-in run context (id + agent + task + repo + barrier), fetched via the
  // real GET /runs/{id} — never invented from the audit event fields, which
  // don't carry them.
  const [drillRun, setDrillRun] = React.useState<AgentRun | undefined>(undefined);
  const [drillLoading, setDrillLoading] = React.useState(false);

  // MEDIUM fix: when a run_id filter is set, query the SERVER with run_id so we
  // get that run's authoritative, complete per-run trail (not a client filter
  // over a truncated global window that may have scrolled the run's events off).
  // MEDIUM fix: do NOT client re-sort by wall-clock time — the server returns
  // events in authoritative append (seq) order; re-sorting by `time` can reorder
  // events that share a timestamp and misrepresent causality. Preserve as-is.
  const load = React.useCallback(() => {
    setStatus("loading");
    api
      .listAudit(runFilter || undefined)
      .then((r) => {
        setEvents(r);
        setTruncated(r.length >= (runFilter ? RUN_AUDIT_PAGE_CAP : AUDIT_PAGE_CAP));
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, [runFilter]);
  React.useEffect(load, [load]);

  // Live-tail: audit is append-only, so a background poll is meaningful (new
  // events only ever get added). Refresh quietly — never flip back to the
  // loading skeleton, that would flicker the whole list every tick.
  const tick = React.useCallback(() => {
    api
      .listAudit(runFilter || undefined)
      .then((r) => {
        setEvents(r);
        setTruncated(r.length >= (runFilter ? RUN_AUDIT_PAGE_CAP : AUDIT_PAGE_CAP));
      })
      .catch(() => {
        /* transient poll failure — keep the last good view, retry next tick */
      });
  }, [runFilter]);
  usePoll(tick, AUDIT_POLL_MS, status !== "ready");

  React.useEffect(() => {
    if (!runFilter) {
      setDrillRun(undefined);
      return;
    }
    let active = true;
    setDrillLoading(true);
    api
      .getRun(runFilter)
      .then((r) => {
        if (active) {
          setDrillRun(r);
          setDrillLoading(false);
        }
      })
      .catch(() => {
        if (active) {
          setDrillRun(undefined);
          setDrillLoading(false);
        }
      });
    return () => {
      active = false;
    };
  }, [runFilter]);

  // Which event-kind facets actually have data in the loaded window — an "only
  // include facets that exist" rule so an operator never sees a dead filter for
  // a kind (e.g. Approvals) this window has none of.
  const presentKinds = React.useMemo(() => {
    const s = new Set<EventKind>();
    for (const e of events) s.add(eventKind(e.action));
    return s;
  }, [events]);

  // Query / kind / actor are sub-filters applied over the loaded window. The
  // run_id filter is enforced server-side (above), so it is not re-applied here.
  const filtered = events.filter((e) => {
    const q = query.trim().toLowerCase();
    const matchQ =
      !q ||
      e.action.toLowerCase().includes(q) ||
      e.actor.toLowerCase().includes(q) ||
      (e.target ?? "").toLowerCase().includes(q) ||
      (e.run_id ?? "").toLowerCase().includes(q);
    const matchKind = kindFilter === "all" || eventKind(e.action) === kindFilter;
    const matchActor = actorFilter === "all" || e.actor_type === actorFilter;
    return matchQ && matchKind && matchActor;
  });

  const groups = React.useMemo(() => groupByDay(filtered), [filtered]);

  const noFiltersActive = !query && kindFilter === "all" && actorFilter === "all" && !runFilter;

  const clearFilters = () => {
    setQuery("");
    setKindFilter("all");
    setActorFilter("all");
    setRunFilter("");
  };

  return (
    <div className="mx-auto max-w-[1400px] px-6 py-6">
      <PageHeader
        title="Audit"
        description="Append-only. Every egress decision, credential broker, approval, and enforcement action — across every run."
      />

      <div className="mb-4 flex flex-wrap items-center gap-3">
        <div className="relative w-full max-w-sm">
          <Search className="absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            placeholder="Search events, domains, run IDs…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            className="pl-9"
          />
        </div>

        <Select value={kindFilter} onValueChange={(v) => setKindFilter(v as EventKind | "all")}>
          <SelectTrigger size="sm" className="w-[150px]" aria-label="Event">
            <SelectValue placeholder="Event" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All events</SelectItem>
            {KIND_ORDER.filter((k) => presentKinds.has(k)).map((k) => (
              <SelectItem key={k} value={k}>
                {KIND_META[k].label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        <Select value={actorFilter} onValueChange={(v) => setActorFilter(v as ActorType | "all")}>
          <SelectTrigger size="sm" className="w-[130px]" aria-label="Actor">
            <SelectValue placeholder="Actor" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All actors</SelectItem>
            <SelectItem value="human">Human</SelectItem>
            <SelectItem value="agent">Agent</SelectItem>
            <SelectItem value="system">System</SelectItem>
          </SelectContent>
        </Select>

        <div className="ml-auto flex items-center gap-3">
          {status === "ready" && (
            <Chip tone="success" dot pulse title="Polling for new events">
              Live · appending
            </Chip>
          )}
          <span className="text-sm text-muted-foreground">
            {status === "ready" && `${filtered.length} event${filtered.length === 1 ? "" : "s"}`}
          </span>
        </div>
      </div>

      {runFilter && (
        <DrillBanner
          runId={runFilter}
          run={drillRun}
          loading={drillLoading}
          onClear={() => setRunFilter("")}
        />
      )}

      {/* MEDIUM/M20 fix: when the window is capped, say so explicitly — otherwise
          the operator may believe they're seeing the full log when they're not.
          Filtering by a specific run pulls that run's complete trail server-side
          (a HIGHER cap, RUN_AUDIT_PAGE_CAP, than the global page), but a long-
          running/chatty run can still hit ITS cap — this used to be suppressed
          unconditionally whenever runFilter was set, silently hiding that this
          run's own trail was cut off too. Only the real page cap is stated — no
          invented retention/pruning numbers. */}
      {status === "ready" && truncated && (
        <div className="mb-4 flex items-center gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
          <AlertTriangle className="size-3.5 shrink-0" />
          <span>
            {runFilter ? (
              <>
                Showing the first {RUN_AUDIT_PAGE_CAP} events for this run (truncated) — older events for
                this run may have rolled off.
              </>
            ) : (
              <>
                Showing the first {AUDIT_PAGE_CAP} events (truncated) — older events may have rolled off this
                window. Filter by a run to see that run's full trail (up to {RUN_AUDIT_PAGE_CAP} events).
              </>
            )}
          </span>
        </div>
      )}

      {status === "loading" ? (
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          <TableSkeleton rows={8} cols={5} />
        </div>
      ) : status === "error" ? (
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          <ErrorState onRetry={load} />
        </div>
      ) : filtered.length === 0 ? (
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          {noFiltersActive ? (
            <EmptyState
              icon={ScrollText}
              title="The trail starts with your first run."
              description="Every egress decision, credential broker, approval, and enforcement action gets recorded here the moment you launch a run."
              action={
                <Button onClick={() => setNewRunOpen(true)}>
                  <Plus className="size-4" /> Launch your first run
                </Button>
              }
            />
          ) : (
            <EmptyState
              icon={ScrollText}
              title="No events match these filters."
              description="Try a different search term or facet."
              action={
                <Button variant="outline" onClick={clearFilters}>
                  Clear filters
                </Button>
              }
            />
          )}
        </div>
      ) : (
        <div className="space-y-4">
          {groups.map((g) => (
            <div key={g.key} className="overflow-hidden rounded-xl border border-border bg-card">
              <div className="border-b border-border bg-surface-2/60 px-4 py-2">
                <SectionLabel>{g.label}</SectionLabel>
              </div>
              <div className="divide-y divide-border">
                {g.events.map((e) => (
                  <EventRow key={e.id} event={e} onDrill={setRunFilter} />
                ))}
              </div>
            </div>
          ))}
        </div>
      )}

      <NewRunDialog
        open={newRunOpen}
        onOpenChange={setNewRunOpen}
        onCreated={() => {
          setNewRunOpen(false);
          load();
        }}
      />
    </div>
  );
}

// Per-run drill banner: real run fields only (id / agent / task / repo / the
// metals ConfinementChip). There is no per-run detail ROUTE in this console yet
// (RunDetail opens as a panel from local state on the Runs screen) — "Open run"
// honestly points at /runs rather than inventing a /runs/:id deep link.
function DrillBanner({
  runId,
  run,
  loading,
  onClear,
}: {
  runId: string;
  run: AgentRun | undefined;
  loading: boolean;
  onClear: () => void;
}) {
  return (
    <div className="mb-4 flex flex-wrap items-center gap-3 rounded-xl border border-primary/25 bg-primary/5 px-4 py-3">
      <Mono className="text-sm text-foreground" title={runId}>
        {runId.replace("run_", "")}
      </Mono>
      {loading ? (
        <span className="text-xs text-muted-foreground">Loading run…</span>
      ) : run ? (
        <>
          <AgentBadge agent={run.agent} />
          <span className="max-w-[280px] truncate text-sm text-muted-foreground" title={run.task}>
            {run.task}
          </span>
          <span className="font-mono text-xs text-muted-foreground">{run.repo}</span>
          <ConfinementChip value={run.confinement_class} />
        </>
      ) : (
        <span className="text-xs text-muted-foreground">Run not found — it may have been archived or deleted.</span>
      )}
      <div className="ml-auto flex items-center gap-4">
        <Link
          to="/runs"
          className="inline-flex items-center gap-1 text-sm font-medium text-primary hover:underline"
        >
          Open run <ArrowRight className="size-3.5" />
        </Link>
        <button onClick={onClear} className="text-sm text-muted-foreground hover:text-foreground">
          Show all events
        </button>
      </div>
    </div>
  );
}

// Outcome, rendered per the event's real kind — never a fabricated field:
//  - run.kill is the ONE solid-red enforcement chip (saturated red reserved for it)
//  - egress.* renders its decision (allow/deny/pending) as mono tinted text
//  - a successful credential.mint reads as "brokered" (still outcome=success on
//    the wire — this is just the honest word for what that success means)
//  - approval.decide reads as approved/denied with an icon
//  - everything else falls back to the shared OutcomeBadge (success/denied/failure)
function EventOutcome({ e }: { e: AuditEvent }) {
  if (e.action === "run.kill") {
    return <Chip tone="danger" className="border-danger bg-danger text-danger-foreground">Killed</Chip>;
  }
  if (e.action.startsWith("egress.")) {
    const decision = e.action.slice("egress.".length);
    const tone =
      decision === "allow" ? "text-success" : decision === "deny" ? "text-danger" : "text-warning";
    return <span className={cn("font-mono text-xs font-semibold", tone)}>{decision}</span>;
  }
  if (e.action === "credential.mint" && e.outcome === "success") {
    return <span className="font-mono text-xs font-semibold text-info">brokered</span>;
  }
  if (e.action === "approval.decide") {
    // The backend writes outcome:"success" for EVERY approval.decide — approve OR
    // deny — so the real verdict lives in data.decision. Branch on that; a denial
    // rendered as a green ✓ approved would falsify the audit record.
    const decision = e.data?.decision;
    if (decision === "DENIED") {
      return (
        <span className="inline-flex items-center gap-1 text-xs font-medium text-danger">
          <CircleX className="size-3.5" /> denied
        </span>
      );
    }
    if (decision === "APPROVED") {
      return (
        <span className="inline-flex items-center gap-1 text-xs font-medium text-success">
          <CircleCheck className="size-3.5" /> approved
        </span>
      );
    }
    return <OutcomeBadge outcome={e.outcome} />;
  }
  return <OutcomeBadge outcome={e.outcome} />;
}

function EventRow({ event, onDrill }: { event: AuditEvent; onDrill: (runId: string) => void }) {
  const kind = eventKind(event.action);
  const Icon = KIND_META[kind].Icon;
  return (
    <div className="flex items-center gap-3 px-4 py-3 hover:bg-surface-2/50">
      <span
        className="w-[84px] shrink-0 font-mono text-xs text-muted-foreground"
        title={absoluteTime(event.time)}
      >
        {new Date(event.time).toLocaleTimeString(undefined, {
          hour: "2-digit",
          minute: "2-digit",
          second: "2-digit",
        })}
      </span>
      <ActorTypeChip type={event.actor_type} />
      <Icon className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
      <span className="min-w-0 flex-1 truncate text-sm text-foreground" title={describeEvent(event)}>
        {describeEvent(event)}
      </span>
      {event.run_id && (
        <button
          onClick={() => onDrill(event.run_id!)}
          className="inline-flex shrink-0 items-center gap-1 rounded-md border border-solid border-border bg-surface-2 px-1.5 py-0.5 font-mono text-[11px] text-muted-foreground hover:border-primary/40 hover:text-primary"
        >
          {event.run_id.replace("run_", "")}
        </button>
      )}
      <div className="shrink-0">
        <EventOutcome e={event} />
      </div>
    </div>
  );
}
