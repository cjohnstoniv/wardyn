/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// UNIFIED RUNS — the merge of the old Runs table and the retired Fleet board.
// One screen, two densities:
//   - Board: the live agent-fleet card board (auto-refreshed every ~3s), grouped
//     Needs-attention / Active / Done-by-outcome.
//   - Table: the same runs in a dense, horizontally-scrollable table.
// Every card / row navigates to the addressable /runs/:id detail page — the old
// slide-over Sheet is gone. "New run" lives in the app shell top bar.
import * as React from "react";
import { useLocation, useNavigate } from "react-router-dom";
import {
  Activity,
  Archive,
  BellRing,
  Check,
  CircleDot,
  Eye,
  FilterX,
  GitBranch,
  LayoutGrid,
  MoreHorizontal,
  Plus,
  RotateCw,
  Rows3,
  Search,
  ShieldX,
  Skull,
  Square,
  TerminalSquare,
} from "lucide-react";
import { toast } from "sonner";
import type { AgentRun, RunState } from "../../lib/types";
import { isTerminalRunState } from "../../lib/types";
import { runs as api } from "../../lib/api/runs";
import { usePoll } from "../../lib/use-poll";
import { getErrorMessage, relativeTime } from "../../lib/format";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../ui/select";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "../ui/dropdown-menu";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../ui/alert-dialog";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../ui/table";
import { AgentBadge, ConfinementChip } from "../wardyn/primitives";
import { BarrierStrengthStrip } from "../wardyn/barrier-strength-strip";
import { RunStatusBadge } from "../wardyn/run-status-badge";
import { Mono } from "../wardyn/code-block";
import { EmptyState, ErrorState } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { cn } from "../ui/utils";

// Runs is the eager landing route, so an eager wizard import would park the
// whole new-run graph (workspaces + secrets screens and their dialogs) in the
// entry chunk for every operator who never clicks "New run". Fetched on the
// click instead; rollup shares the chunk with the other mount sites.
const NewRunDialog = React.lazy(() => import("./new-run/new-run-dialog").then((m) => ({ default: m.NewRunDialog })));

// Live-board refresh cadence. Matches the retired Fleet board — a live board
// shouldn't need a manual reload to feel alive.
const POLL_MS = 3000;

// Which states need an operator's eyes — mirrors App.tsx ATTENTION_STATES (the
// sidebar amber badge). FAILED needs review; WAITING_FOR_CONFIRMATION needs a
// click to unblock the agent. Runs in these states are lifted OUT of Active/Done
// into the amber "Needs your attention" section (so they never render twice).
const ATTENTION_STATES = new Set<string>(["FAILED", "WAITING_FOR_CONFIRMATION"]);

// Done section, grouped by terminal outcome. FAILED is intentionally absent — a
// failed run is surfaced under "Needs your attention" instead (sidebar mirror).
const DONE_GROUPS: { key: string; label: string; Icon: React.ElementType; tint: string }[] = [
  { key: "COMPLETED", label: "Completed", Icon: Check, tint: "text-success" },
  { key: "STOPPED", label: "Stopped", Icon: Square, tint: "text-muted-foreground" },
  { key: "KILLED", label: "Killed", Icon: ShieldX, tint: "text-danger" },
  { key: "ARCHIVED", label: "Archived", Icon: Archive, tint: "text-muted-foreground" },
];

// Per-outcome-group collapsed preview before "Show all N".
const GROUP_PREVIEW = 3;
// Table display cap (client-side; listRuns returns the full set) + load-more step.
const TABLE_STEP = 25;

type Mode = "board" | "table";
type StateFacet = "all" | "attention" | "active" | "done";

export function RunsScreen() {
  const navigate = useNavigate();
  const [runs, setRuns] = React.useState<AgentRun[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [mode, setMode] = React.useState<Mode>("board");
  const [query, setQuery] = React.useState("");
  const [stateFacet, setStateFacet] = React.useState<StateFacet>("all");
  const [agentFacet, setAgentFacet] = React.useState<string>("all");
  const [repoFacet, setRepoFacet] = React.useState<string>("all");
  const [expanded, setExpanded] = React.useState<Record<string, boolean>>({});
  const [tableCap, setTableCap] = React.useState(TABLE_STEP);
  const [newOpen, setNewOpen] = React.useState(false);
  // Latches on the first open and never resets, so the lazy dialog below keeps
  // its close animation and internal wizard state across dismissals.
  const [newMounted, setNewMounted] = React.useState(false);

  const fetchRuns = React.useCallback(() => {
    return api.listRuns().then((r) => {
      setRuns(r);
      setStatus("ready");
    });
  }, []);

  // Foreground load: flips the skeleton / error state.
  const load = React.useCallback(() => {
    setStatus("loading");
    fetchRuns().catch(() => setStatus("error"));
  }, [fetchRuns]);

  // Reload on every navigation to /runs (location.key changes even same-path) so
  // the shell's "New run" — which navigates here after a create from any screen —
  // always surfaces the new run.
  const location = useLocation();
  React.useEffect(load, [load, location.key]);

  // Background refresh: update in place, silent on failure (a blip shouldn't
  // blow the board away — keep last-good data and recover next tick).
  const refresh = React.useCallback(() => {
    fetchRuns().catch(() => {
      /* keep last-good data */
    });
  }, [fetchRuns]);
  usePoll(refresh, POLL_MS, newOpen);

  const kill = async (id: string) => {
    try {
      await api.killRun(id);
      toast.success(`Kill requested for ${id}`);
    } catch (err) {
      toast.error(`Failed to kill ${id}`, {
        description: getErrorMessage(err),
      });
    } finally {
      refresh();
    }
  };

  // Facet option lists — derived from REAL loaded runs, never a fixed mock set.
  // Facet options must be NON-EMPTY: an ephemeral run has repo="" (and a run could
  // in principle carry an empty agent), and a Radix <SelectItem value=""> throws
  // ("must have a value prop that is not an empty string"), crashing the whole Runs
  // page. Drop empties — "All" already covers those runs.
  const agents = React.useMemo(
    () => Array.from(new Set(runs.map((r) => r.agent).filter(Boolean))).sort(),
    [runs],
  );
  const repos = React.useMemo(
    () => Array.from(new Set(runs.map((r) => r.repo).filter(Boolean))).sort(),
    [runs],
  );

  const q = query.trim().toLowerCase();
  const filtered = runs.filter((r) => {
    if (agentFacet !== "all" && r.agent !== agentFacet) return false;
    if (repoFacet !== "all" && r.repo !== repoFacet) return false;
    if (
      q &&
      !(
        r.id.toLowerCase().includes(q) ||
        r.agent.toLowerCase().includes(q) ||
        r.repo.toLowerCase().includes(q) ||
        (r.workspace_path ?? "").toLowerCase().includes(q) ||
        r.task.toLowerCase().includes(q) ||
        r.created_by.toLowerCase().includes(q)
      )
    ) {
      return false;
    }
    return true;
  });

  const attention = filtered.filter((r) => ATTENTION_STATES.has(r.state as string));
  const active = filtered.filter(
    (r) => !isTerminalRunState(r.state) && !ATTENTION_STATES.has(r.state as string),
  );
  const done = filtered
    .filter((r) => isTerminalRunState(r.state) && !ATTENTION_STATES.has(r.state as string))
    .sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));

  const trueEmpty = status === "ready" && runs.length === 0;

  // The state facet gates which sections are in scope. Derive the facet-filtered
  // sets ONCE so both the board sections AND the table rows honor it — and so the
  // empty state fires when a facet (not just search) hides everything.
  const facetAttention = stateFacet === "all" || stateFacet === "attention" ? attention : [];
  const facetActive = stateFacet === "all" || stateFacet === "active" ? active : [];
  const facetDone = stateFacet === "all" || stateFacet === "done" ? done : [];
  const visibleCount = facetAttention.length + facetActive.length + facetDone.length;
  const noMatches = status === "ready" && !trueEmpty && visibleCount === 0;

  const showAttention = facetAttention.length > 0;
  const showActive = facetActive.length > 0;
  const showDone = facetDone.length > 0;

  const openRun = (id: string) => navigate(`/runs/${encodeURIComponent(id)}`);

  const clearFilters = () => {
    setQuery("");
    setStateFacet("all");
    setAgentFacet("all");
    setRepoFacet("all");
  };

  return (
    <div className="mx-auto max-w-[1400px] px-6 py-6">
      <PageHeader
        title="Runs"
        description="Every run, live — each confined behind its own barrier."
        actions={
          <div className="inline-flex gap-1 rounded-lg border border-border bg-surface-2/60 p-1">
            <DensityButton active={mode === "board"} onClick={() => setMode("board")} Icon={LayoutGrid}>
              Board
            </DensityButton>
            <DensityButton active={mode === "table"} onClick={() => setMode("table")} Icon={Rows3}>
              Table
            </DensityButton>
          </div>
        }
      />

      {status === "ready" && !trueEmpty && (
        <div className="mb-5 flex flex-wrap items-center gap-3">
          <div className="relative w-full max-w-xs">
            <Search className="absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              placeholder="Search runs, repos, IDs…"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              className="pl-9"
            />
          </div>

          <Select value={stateFacet} onValueChange={(v) => setStateFacet(v as StateFacet)}>
            <SelectTrigger size="sm" className="w-[150px]" aria-label="State">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">State · All</SelectItem>
              <SelectItem value="attention">Needs attention</SelectItem>
              <SelectItem value="active">Active</SelectItem>
              <SelectItem value="done">Done</SelectItem>
            </SelectContent>
          </Select>

          <Select value={agentFacet} onValueChange={setAgentFacet}>
            <SelectTrigger size="sm" className="w-[150px]" aria-label="Agent">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">Agent · All</SelectItem>
              {agents.map((a) => (
                <SelectItem key={a} value={a}>
                  {a}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>

          <Select value={repoFacet} onValueChange={setRepoFacet}>
            <SelectTrigger size="sm" className="w-[170px]" aria-label="Repo">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">Repo · All</SelectItem>
              {repos.map((r) => (
                <SelectItem key={r} value={r}>
                  {r}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>

          <span className="ml-auto inline-flex items-center gap-1.5 text-xs text-muted-foreground">
            <CircleDot className="size-3 animate-pulse text-success" />
            Live · refreshes every {POLL_MS / 1000}s
          </span>
          <Button variant="outline" size="icon" onClick={load} aria-label="Refresh now">
            <RotateCw className="size-4" />
          </Button>
        </div>
      )}

      {status === "loading" ? (
        <BoardSkeleton />
      ) : status === "error" ? (
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          <ErrorState onRetry={load} />
        </div>
      ) : trueEmpty ? (
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          <EmptyState
            icon={Activity}
            title="No runs yet."
            description="Launch your first run and watch it here live — confined behind its barrier, gated by approvals, recorded end to end."
            action={
              <Button
                onClick={() => {
                  setNewMounted(true);
                  setNewOpen(true);
                }}
              >
                <Plus className="size-4" /> Launch your first run
              </Button>
            }
          />
        </div>
      ) : noMatches ? (
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          <EmptyState
            icon={FilterX}
            title="No runs match these filters."
            description="Try a different search term or facet."
            action={
              <Button variant="outline" onClick={clearFilters}>
                Clear filters
              </Button>
            }
          />
        </div>
      ) : mode === "board" ? (
        <div className="space-y-7">
          {showAttention && (
            <section aria-label="Needs your attention">
              <SectionHeading
                Icon={BellRing}
                iconTint="text-warning"
                title="Needs your attention"
                titleTint="text-warning"
                count={attention.length}
                countTint="warning"
              />
              <CardGrid>
                {attention.map((run) => (
                  <RunCard key={run.id} run={run} attention onOpen={openRun} onKill={kill} />
                ))}
              </CardGrid>
            </section>
          )}

          {showActive && (
            <section aria-label="Active">
              <SectionHeading title="Active" count={active.length} />
              <CardGrid>
                {active.map((run) => (
                  <RunCard key={run.id} run={run} onOpen={openRun} onKill={kill} />
                ))}
              </CardGrid>
            </section>
          )}

          {showDone && (
            <DoneSection
              done={done}
              expanded={expanded}
              setExpanded={setExpanded}
              onOpen={openRun}
              onKill={kill}
            />
          )}
        </div>
      ) : (
        <RunsTable
          rows={[...facetAttention, ...facetActive, ...facetDone]}
          cap={tableCap}
          onLoadMore={() => setTableCap((c) => c + TABLE_STEP)}
          onOpen={openRun}
          onKill={kill}
        />
      )}

      {newMounted && (
        <React.Suspense fallback={null}>
          <NewRunDialog open={newOpen} onOpenChange={setNewOpen} onCreated={(r) => openRun(r.id)} />
        </React.Suspense>
      )}
    </div>
  );
}

function DensityButton({
  active,
  onClick,
  Icon,
  children,
}: {
  active: boolean;
  onClick: () => void;
  Icon: React.ElementType;
  children: React.ReactNode;
}) {
  return (
    <button
      onClick={onClick}
      aria-pressed={active}
      className={cn(
        "inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 text-sm font-medium transition-colors",
        active ? "bg-card text-foreground shadow-sm" : "text-muted-foreground hover:text-foreground",
      )}
    >
      <Icon className="size-3.5" />
      {children}
    </button>
  );
}

function SectionHeading({
  Icon,
  iconTint,
  title,
  titleTint = "text-muted-foreground",
  count,
  countTint = "neutral",
  hint,
}: {
  Icon?: React.ElementType;
  iconTint?: string;
  title: string;
  titleTint?: string;
  count: number;
  countTint?: "neutral" | "warning";
  hint?: string;
}) {
  return (
    <div className="mb-3 flex items-center gap-2">
      {Icon && <Icon className={cn("size-3.5", iconTint)} />}
      <h2 className={cn("text-[11px] font-semibold uppercase tracking-wider", titleTint)}>{title}</h2>
      <span
        className={cn(
          "rounded-full px-1.5 text-[11px] font-semibold",
          countTint === "warning"
            ? "bg-warning-subtle text-warning"
            : "bg-muted text-muted-foreground",
        )}
      >
        {count}
      </span>
      {hint && <span className="text-xs text-muted-foreground">{hint}</span>}
    </div>
  );
}

function CardGrid({ children }: { children: React.ReactNode }) {
  // auto-fill with a min(100%, floor) track: cards reflow and collapse to ONE
  // column below ~1100px instead of clipping (min(100%, …) stops overflow on
  // narrow containers).
  return (
    <div className="grid gap-3 grid-cols-[repeat(auto-fill,minmax(min(100%,34rem),1fr))]">
      {children}
    </div>
  );
}

function DoneSection({
  done,
  expanded,
  setExpanded,
  onOpen,
  onKill,
}: {
  done: AgentRun[];
  expanded: Record<string, boolean>;
  setExpanded: React.Dispatch<React.SetStateAction<Record<string, boolean>>>;
  onOpen: (id: string) => void;
  onKill: (id: string) => void;
}) {
  const groups = DONE_GROUPS.map((g) => ({
    ...g,
    runs: done.filter((r) => (r.state as string) === g.key),
  })).filter((g) => g.runs.length > 0);

  const shownCount = groups.reduce(
    (n, g) => n + (expanded[g.key] ? g.runs.length : Math.min(GROUP_PREVIEW, g.runs.length)),
    0,
  );
  const allExpanded = shownCount >= done.length;
  const hasHidden = done.length > shownCount;

  const toggleAll = () =>
    setExpanded(allExpanded ? {} : Object.fromEntries(groups.map((g) => [g.key, true])));

  return (
    <section aria-label="Done">
      <SectionHeading title="Done" count={done.length} hint="grouped by outcome" />
      <div className="space-y-5">
        {groups.map((g) => {
          const isOpen = !!expanded[g.key];
          const shown = isOpen ? g.runs : g.runs.slice(0, GROUP_PREVIEW);
          return (
            <div key={g.key}>
              <div className="mb-2.5 flex items-center gap-2">
                <g.Icon className={cn("size-3.5", g.tint)} />
                <span className="text-[12.5px] font-semibold text-foreground">{g.label}</span>
                <span className="text-xs text-muted-foreground">{g.runs.length}</span>
                {g.runs.length > GROUP_PREVIEW && (
                  <button
                    onClick={() =>
                      setExpanded((s) => ({ ...s, [g.key]: !s[g.key] }))
                    }
                    className="ml-1 text-xs font-medium text-primary hover:underline"
                  >
                    {isOpen ? "Show fewer" : `Show all ${g.runs.length}`}
                  </button>
                )}
              </div>
              <CardGrid>
                {shown.map((run) => (
                  <RunCard key={run.id} run={run} done onOpen={onOpen} onKill={onKill} />
                ))}
              </CardGrid>
            </div>
          );
        })}
      </div>
      <div className="mt-5 flex items-center justify-center gap-3">
        <span className="text-xs text-muted-foreground">
          Showing {shownCount} of {done.length} done runs
        </span>
        {(hasHidden || allExpanded) && done.length > GROUP_PREVIEW && (
          <Button variant="outline" size="sm" onClick={toggleAll}>
            {allExpanded ? "Show fewer" : "Show all"}
          </Button>
        )}
      </div>
    </section>
  );
}

function RunCard({
  run,
  attention,
  done,
  onOpen,
  onKill,
}: {
  run: AgentRun;
  attention?: boolean;
  done?: boolean;
  onOpen: (id: string) => void;
  onKill: (id: string) => void;
}) {
  const terminal = isTerminalRunState(run.state);
  const attachable = !!run.interactive && run.state === "RUNNING";
  const note = attentionNote(run.state);

  return (
    <div
      role="button"
      tabIndex={0}
      onClick={() => onOpen(run.id)}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onOpen(run.id);
        }
      }}
      className={cn(
        "group relative flex cursor-pointer flex-col gap-3 rounded-xl border bg-card p-4 text-left transition-colors hover:border-border-strong",
        attention ? "border-warning/30" : "border-border",
        done && "opacity-90",
      )}
    >
      {attention && (
        <span className="absolute inset-y-3 left-0 w-0.5 rounded-r bg-warning" aria-hidden="true" />
      )}

      <div className="flex items-start gap-2.5">
        <AgentBadge agent={run.agent} withLabel={false} />
        <p className="min-w-0 flex-1 text-sm font-medium leading-snug text-foreground">
          {run.task || "—"}
        </p>
        <RunActions run={run} terminal={terminal} attachable={attachable} onOpen={onOpen} onKill={onKill} />
      </div>

      <div className="flex min-w-0 items-center gap-1.5 text-xs text-muted-foreground">
        <GitBranch className="size-3.5 shrink-0" />
        <span className="truncate font-mono" title={run.workspace_path || run.repo}>
          {run.repo}
        </span>
      </div>

      {note && (
        <div className="flex items-center gap-1.5 text-[12.5px] text-warning">
          <BellRing className="size-3.5 shrink-0" />
          <span>{note}</span>
        </div>
      )}

      <div className="mt-auto flex flex-wrap items-center gap-x-2 gap-y-1.5 border-t border-border pt-3">
        <RunStatusBadge state={run.state} />
        <ConfinementChip value={run.confinement_class} />
        <BarrierStrengthStrip tier={run.confinement_class} muted={done} />
        <div className="ml-auto flex items-center gap-2.5">
          {attachable && (
            <Button
              size="sm"
              variant="outline"
              onClick={(e) => {
                e.stopPropagation();
                onOpen(run.id);
              }}
            >
              <TerminalSquare className="size-3.5" /> Attach
            </Button>
          )}
          {attention && (
            <Button
              size="sm"
              onClick={(e) => {
                e.stopPropagation();
                onOpen(run.id);
              }}
            >
              Review
            </Button>
          )}
          <Mono className="max-w-[8rem] truncate text-[11px]" title={run.id}>
            {shortId(run.id)}
          </Mono>
          <span className="whitespace-nowrap text-[11px] text-muted-foreground" title={run.created_at}>
            {relativeTime(run.created_at)}
          </span>
        </div>
      </div>
    </div>
  );
}

function RunsTable({
  rows,
  cap,
  onLoadMore,
  onOpen,
  onKill,
}: {
  rows: AgentRun[];
  cap: number;
  onLoadMore: () => void;
  onOpen: (id: string) => void;
  onKill: (id: string) => void;
}) {
  const shown = rows.slice(0, cap);
  return (
    <div className="overflow-hidden rounded-xl border border-border bg-card">
      <Table className="min-w-[960px]">
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>Run</TableHead>
            <TableHead className="w-[180px]">State</TableHead>
            <TableHead className="w-[130px]">Barrier</TableHead>
            <TableHead className="w-[180px]">Repo</TableHead>
            <TableHead className="w-[220px]">Run ID</TableHead>
            <TableHead className="w-[110px]">Created</TableHead>
            <TableHead className="w-[44px]" />
          </TableRow>
        </TableHeader>
        <TableBody>
          {shown.map((run) => {
            const terminal = isTerminalRunState(run.state);
            const attachable = !!run.interactive && run.state === "RUNNING";
            return (
              <TableRow
                key={run.id}
                role="button"
                tabIndex={0}
                onClick={() => onOpen(run.id)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    onOpen(run.id);
                  }
                }}
                className="cursor-pointer"
              >
                <TableCell>
                  <div className="flex min-w-0 items-center gap-2.5">
                    <AgentBadge agent={run.agent} withLabel={false} />
                    <span className="block max-w-[320px] truncate text-sm font-medium text-foreground">
                      {run.task || "—"}
                    </span>
                  </div>
                </TableCell>
                <TableCell>
                  <RunStatusBadge state={run.state} />
                </TableCell>
                <TableCell>
                  <ConfinementChip value={run.confinement_class} />
                </TableCell>
                <TableCell>
                  <span className="whitespace-nowrap font-mono text-xs text-muted-foreground">{run.repo}</span>
                </TableCell>
                <TableCell>
                  {/* Run ID never truncates — it stays fully readable and the table
                      scrolls horizontally instead. */}
                  <span className="whitespace-nowrap font-mono text-[11.5px] text-muted-foreground">{run.id}</span>
                </TableCell>
                <TableCell>
                  <span className="whitespace-nowrap text-xs text-muted-foreground" title={run.created_at}>
                    {relativeTime(run.created_at)}
                  </span>
                </TableCell>
                <TableCell onClick={(e) => e.stopPropagation()}>
                  <RunActions run={run} terminal={terminal} attachable={attachable} onOpen={onOpen} onKill={onKill} />
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
      <div className="flex items-center justify-between gap-3 border-t border-border bg-surface-2/40 px-4 py-2.5">
        <span className="text-xs text-muted-foreground">
          Narrow screens scroll horizontally — the Run ID never truncates.
        </span>
        <span className="text-xs text-muted-foreground">
          {Math.min(cap, rows.length)} of {rows.length}
          {rows.length > cap && (
            <>
              {" · "}
              <button onClick={onLoadMore} className="font-medium text-primary hover:underline">
                Load {TABLE_STEP} more
              </button>
            </>
          )}
        </span>
      </div>
    </div>
  );
}

function RunActions({
  run,
  terminal,
  attachable,
  onOpen,
  onKill,
}: {
  run: AgentRun;
  terminal: boolean;
  attachable: boolean;
  onOpen: (id: string) => void;
  onKill: (id: string) => void;
}) {
  // fix: the board's Kill action fired with no confirmation, unlike the
  // identical action on Run Detail (AlertDialog-gated) — one misclick here
  // killed a run with zero chance to back out. The dialog is rendered as a
  // SIBLING of DropdownMenuContent (not nested inside it), controlled by its
  // own state, so it survives the menu's close/unmount.
  const [confirmOpen, setConfirmOpen] = React.useState(false);
  return (
    <div onClick={(e) => e.stopPropagation()}>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="icon" className="size-8" aria-label="Run actions">
            <MoreHorizontal className="size-4" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onClick={() => onOpen(run.id)}>
            <Eye className="size-4" /> Open detail
          </DropdownMenuItem>
          {attachable && (
            <DropdownMenuItem onClick={() => onOpen(run.id)}>
              <TerminalSquare className="size-4" /> Attach
            </DropdownMenuItem>
          )}
          <DropdownMenuSeparator />
          <DropdownMenuItem
            disabled={terminal}
            onClick={() => setConfirmOpen(true)}
            className="text-danger focus:text-danger"
          >
            <Skull className="size-4" /> Kill run
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>

      <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
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
            <AlertDialogAction
              onClick={() => {
                setConfirmOpen(false);
                onKill(run.id);
              }}
              className="bg-destructive text-white hover:bg-destructive/90"
            >
              Kill run
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function BoardSkeleton() {
  return (
    <div className="grid gap-3 grid-cols-[repeat(auto-fill,minmax(min(100%,34rem),1fr))]">
      {Array.from({ length: 6 }).map((_, i) => (
        <div key={i} className="space-y-3 rounded-xl border border-border bg-card p-4">
          <div className="h-6 w-28 animate-pulse rounded bg-muted" />
          <div className="h-3.5 w-40 animate-pulse rounded bg-muted" />
          <div className="h-3.5 w-full animate-pulse rounded bg-muted" />
          <div className="flex gap-2">
            <div className="h-5 w-20 animate-pulse rounded bg-muted" />
            <div className="h-5 w-12 animate-pulse rounded bg-muted" />
          </div>
        </div>
      ))}
    </div>
  );
}

function attentionNote(state: RunState): string | null {
  if (state === "WAITING_FOR_CONFIRMATION") return "Waiting for your confirmation";
  if (state === "FAILED") return "Run failed — review what happened";
  return null;
}

function shortId(id: string): string {
  const base = id.replace(/^run_/, "");
  return base.length > 10 ? base.slice(0, 8) + "…" : base;
}
