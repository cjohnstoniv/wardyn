/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// UNIFIED RUNS — one screen, two densities:
//   - Board: the live run card board (auto-refreshed every ~3s), grouped by the
//     run's TITLE — runs that share one are the same piece of work.
//   - Table: the same runs and the same groups, dense and horizontally
//     scrollable, with a header row per group.
// This used to group by state (Needs-attention / Active / Done-by-outcome).
// Titles replaced that as the grouping axis; the triage it provided survives in
// the state facet, in the attention-first ordering, and in each group header's
// per-state counts. See titleGroups.
// Every card / row navigates to the addressable /runs/:id detail page.
// "New run" lives in the app shell top bar.
import * as React from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { BellRing, Eye, FilterX, GitBranch, LayoutGrid, MoreHorizontal, RotateCw, Rows3, Search, Skull, TerminalSquare } from "lucide-react";
import { toast } from "sonner";
import type { AgentRun, RunState, SetupStatus } from "../../lib/types";
import { isTerminalRunState, runHeadline } from "../../lib/types";
import { runs as api } from "../../lib/api/runs";
import { setup as setupApi } from "../../lib/api/setup";
import { LIST_LIMIT } from "../../lib/api/core";
import { usePoll } from "../../lib/use-poll";
import { getErrorMessage, relativeTime } from "../../lib/format";
import { deriveReadiness } from "../../lib/readiness";
import { NoBarrierBanner, RunsFirstRun } from "./runs-first-run";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "../ui/dropdown-menu";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { AgentBadge, Chip, ConfinementChip, RunStateBadge } from "../wardyn/primitives";
import { BarrierStrengthStrip } from "../wardyn/barrier-strength-strip";
import { KillRunDialog } from "../wardyn/kill-run-dialog";
import { Mono } from "../wardyn/code-block";
import { EmptyState, ErrorState, TableSkeleton, TruncatedNote } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { useRole } from "../wardyn/operator-context";
import { cn } from "../ui/utils";

// Runs is the eager landing route, so an eager wizard import would park the
// whole new-run graph (workspaces + secrets screens and their dialogs) in the
// entry chunk for every operator who never clicks "New run". Fetched on the
// click instead; rollup shares the chunk with the other mount sites.

// Live-board refresh cadence — a live board shouldn't need a manual reload to
// feel alive.
const POLL_MS = 3000;

// How often the first-run checklist / no-barrier blocker re-checks setup status.
const SETUP_POLL_MS = 5000;

// Which states need an operator's eyes — mirrors App.tsx ATTENTION_STATES (the
// sidebar amber badge). FAILED needs review; WAITING_FOR_CONFIRMATION needs a
// click to unblock the agent. Runs in these states are lifted OUT of Active/Done
// into the amber "Needs your attention" section (so they never render twice).
const ATTENTION_STATES = new Set<string>(["FAILED", "WAITING_FOR_CONFIRMATION"]);

// Per-group collapsed preview before "Show all N".
const GROUP_PREVIEW = 3;

// ── title grouping ──────────────────────────────────────────────────────────
// The board groups by the run's TITLE: runs that share one are the same piece
// of work, and seeing the twelve nightly audits as one thing is the point of
// naming them. It replaced grouping by state — the triage that gave up is
// preserved two ways: the state facet still narrows BEFORE grouping, and the
// input list arrives pre-ordered attention → active → done, so a group holding
// a failed run floats to the top for free and its header says so.
//
// A title held by only ONE run is not a group. Those, and every untitled run
// (legacy rows, CLI runs, the server's own system runs), fall into a single
// trailing grid — a header per singleton is noise, not structure.
//
// ponytail: computed over the LOADED page, not the server. A title split across
// a pagination boundary groups per page; add a server-side group-by if run
// counts ever outgrow LIST_LIMIT.
function titleGroups(runs: AgentRun[]): { groups: { title: string; runs: AgentRun[] }[]; loose: AgentRun[] } {
  const by = new Map<string, AgentRun[]>();
  for (const r of runs) {
    const t = (r.title ?? "").trim();
    if (t) by.set(t, [...(by.get(t) ?? []), r]);
  }
  const groups = [...by].filter(([, rs]) => rs.length > 1).map(([title, rs]) => ({ title, runs: rs }));
  const grouped = new Set(groups.flatMap((g) => g.runs.map((r) => r.id)));
  return { groups, loose: runs.filter((r) => !grouped.has(r.id)) };
}

// What one card/row calls itself. INSIDE a group the title is already on the
// header, so the row's job is to distinguish this run from its siblings — the
// task does that; the title would print three identical cards. Outside a group
// the run has to name itself, which is runHeadline's whole purpose.
function rowHeadline(run: AgentRun, grouped: boolean): string {
  if (!grouped) return runHeadline(run);
  return run.task || (run.interactive ? "Interactive session" : "—");
}
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

  // Backs both the first-run checklist (barrier tiers / model provider) and the
  // one hard blocker in the product: no sandbox barrier at all. Polled ONLY
  // while that blocker is up, so the banner clears on its own once
  // `sudo wardyn setup fence` lands, with no manual reload — Re-check just
  // fires it early. On a host with a barrier there is nothing to watch for,
  // and /setup/status is expensive (a full ListRuns plus a shell-out host
  // sweep), so the poll stops rather than running forever on the landing
  // screen of every open tab.
  const [setupStatus, setSetupStatus] = React.useState<SetupStatus | null>(null);
  const loadSetupStatus = React.useCallback(() => setupApi.getSetupStatus().then(setSetupStatus), []);
  React.useEffect(() => {
    void loadSetupStatus();
  }, [loadSetupStatus]);
  const readiness = setupStatus ? deriveReadiness(setupStatus) : null;
  const confinementClasses = setupStatus?.runner?.confinement_classes ?? [];
  // A merely-unreachable daemon (READY_FALLBACK) must never read as "no
  // barrier installed" — that's a connectivity fact, not a host-config one.
  const noBarrier = !!setupStatus && !setupStatus.unreachable && confinementClasses.length === 0;
  // Keep retrying while the daemon isn't answering either, so a blip at mount
  // doesn't strand this screen on the synthetic READY_FALLBACK until a reload.
  usePoll(loadSetupStatus, SETUP_POLL_MS, !noBarrier && !setupStatus?.unreachable);

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

  // workspace-detail's "Start a run" CTA (#10/D14) lands here with route
  // state instead of a stale pre-seed promise — this dialog already opens on
  // the workspace-first picker, so opening it on arrival is the whole fix.
  // Clear the state right after so a back-navigation or refresh can't reopen
  // it a second time.
  React.useEffect(() => {
    const s = location.state as { openNewRun?: boolean } | null;
    if (!s?.openNewRun) return;
    // Was: open the dialog here. New run is its own page now, so the same
    // intent is a redirect — and `replace` keeps Back going where the operator
    // came from rather than bouncing through this screen again.
    navigate("/runs/new", { replace: true });
  }, [location.state, navigate]);

  // Background refresh: update in place, silent on failure (a blip shouldn't
  // blow the board away — keep last-good data and recover next tick).
  const refresh = React.useCallback(() => {
    fetchRuns().catch(() => {
      /* keep last-good data */
    });
  }, [fetchRuns]);
  // Nothing pauses the board any more — the New run dialog that used to was
  // replaced by its own page, which unmounts this screen entirely.
  usePoll(refresh, POLL_MS, false);

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
        (r.title ?? "").toLowerCase().includes(q) ||
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
  // Concatenated in triage order — attention, then active, then done
  // newest-first. Both densities group THIS list, so group ordering and
  // within-group ordering both fall out of it with no comparator: a group
  // holding a failed run necessarily contains the earliest element and sorts
  // first. Keep the concatenation order if you touch this.
  const visible = [...facetAttention, ...facetActive, ...facetDone];
  const noMatches = status === "ready" && !trueEmpty && visible.length === 0;
  const { groups: titled, loose } = titleGroups(visible);

  const openRun = (id: string) => navigate(`/runs/${encodeURIComponent(id)}`);

  const clearFilters = () => {
    setQuery("");
    setStateFacet("all");
    setAgentFacet("all");
    setRepoFacet("all");
  };

  // Member console (B3, prompt-v2 point 2): the list itself is already scoped
  // server-side (handleListRuns's creator-pager branch) — this is copy only,
  // saying plainly what's already true rather than re-deriving/re-filtering
  // anything client-side.
  const role = useRole();
  const description =
    role === "member" ? `Your runs · ${runs.length}` : "Every run, live — each confined behind its own barrier.";

  return (
    <div className="mx-auto max-w-[1400px] px-6 py-6">
      {/* The one hard blocker in the product: with no sandbox barrier, a run
          cannot start at all, regardless of how many already exist — this sits
          above everything else on the page, not just the empty state. */}
      {noBarrier && <NoBarrierBanner onRecheck={loadSetupStatus} />}

      <PageHeader
        title="Runs"
        description={description}
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

          {/* fix: this used to be plain muted text + a raw CircleDot icon —
              a second visual treatment for the same "live" concept Audit
              already renders as a Chip pill. Shared primitive, same "Live ·
              …" copy template. */}
          <Chip tone="success" dot pulse className="ml-auto" title="Polling for new runs">
            Live · refreshes every {POLL_MS / 1000}s
          </Chip>
          <Button variant="outline" size="icon" onClick={load} aria-label="Refresh now">
            <RotateCw className="size-4" />
          </Button>
        </div>
      )}

      {/* Past the cap the facets above filter only the fetched window, so a
          "no runs match" would be a lie — say the window is a window. */}
      <TruncatedNote count={runs.length} cap={LIST_LIMIT} />

      {status === "loading" ? (
        // fix: this used to always render the board card-grid skeleton, even
        // in Table density — flashing the wrong shape on every manual
        // Refresh / re-navigation while Table mode was active.
        mode === "table" ? (
          <div className="overflow-hidden rounded-xl border border-border bg-card">
            <TableSkeleton rows={8} cols={7} />
          </div>
        ) : (
          <BoardSkeleton />
        )
      ) : status === "error" ? (
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          <ErrorState onRetry={load} />
        </div>
      ) : trueEmpty ? (
        <RunsFirstRun
          readiness={readiness}
          confinementClasses={confinementClasses}
          secretNames={setupStatus?.secrets.present ?? []}
          onNewRun={() => navigate("/runs/new")}
        />
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
          {titled.map((g) => (
            <TitleGroup
              key={g.title}
              title={g.title}
              runs={g.runs}
              open={!!expanded[g.title]}
              onToggle={() => setExpanded((s) => ({ ...s, [g.title]: !s[g.title] }))}
              onOpen={openRun}
              onKill={kill}
            />
          ))}

          {loose.length > 0 && (
            <section aria-label="Ungrouped">
              {/* Only labelled when there is something to distinguish it FROM —
                  on a board with no shared titles, "Ungrouped" describes every
                  run on the page and says nothing. */}
              {titled.length > 0 && <SectionHeading title="Ungrouped" count={loose.length} />}
              <CardGrid>
                {loose.map((run) => (
                  <RunCard
                    key={run.id}
                    run={run}
                    attention={ATTENTION_STATES.has(run.state as string)}
                    done={isTerminalRunState(run.state)}
                    onOpen={openRun}
                    onKill={kill}
                  />
                ))}
              </CardGrid>
            </section>
          )}
        </div>
      ) : (
        <RunsTable
          groups={titled}
          loose={loose}
          cap={tableCap}
          onLoadMore={() => setTableCap((c) => c + TABLE_STEP)}
          onOpen={openRun}
          onKill={kill}
        />
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
      <h2 className={cn("text-[0.6875rem] font-semibold uppercase tracking-wider", titleTint)}>{title}</h2>
      <span
        className={cn(
          "rounded-full px-1.5 text-[0.6875rem] font-semibold",
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

// One title's runs: the board's grouping unit now that runs are named.
//
// The header carries per-state counts, which is what makes replacing the old
// Needs-attention / Active / Done sections honest — the triage those sections
// provided is still legible here, per group, instead of splitting one piece of
// work across three places on the page.
function TitleGroup({
  title,
  runs,
  open,
  onToggle,
  onOpen,
  onKill,
}: {
  title: string;
  runs: AgentRun[];
  open: boolean;
  onToggle: () => void;
  onOpen: (id: string) => void;
  onKill: (id: string) => void;
}) {
  // Distinct states in the order they appear — which is triage order, since
  // `visible` arrives attention → active → done (see its comment).
  const states: string[] = [];
  for (const r of runs) if (!states.includes(r.state as string)) states.push(r.state as string);
  const needsEyes = runs.some((r) => ATTENTION_STATES.has(r.state as string));
  const shown = open ? runs : runs.slice(0, GROUP_PREVIEW);

  return (
    <section aria-label={title}>
      <div className="mb-3 flex flex-wrap items-center gap-2">
        {needsEyes && <BellRing className="size-3.5 text-warning" aria-hidden="true" />}
        <h2
          className={cn(
            "max-w-[420px] truncate text-[0.8125rem] font-semibold",
            needsEyes ? "text-warning" : "text-foreground",
          )}
          title={title}
        >
          {title}
        </h2>
        <span className="rounded-full bg-muted px-1.5 text-[0.6875rem] font-semibold text-muted-foreground">
          {runs.length}
        </span>
        <span className="flex flex-wrap items-center gap-1.5">
          {states.map((st) => {
            const n = runs.filter((r) => (r.state as string) === st).length;
            return (
              <span key={st} className="flex items-center gap-1">
                <RunStateBadge state={st} />
                {n > 1 && <span className="text-[0.6875rem] text-muted-foreground">×{n}</span>}
              </span>
            );
          })}
        </span>
        {runs.length > GROUP_PREVIEW && (
          <button onClick={onToggle} className="ml-1 text-xs font-medium text-primary hover:underline">
            {open ? "Show fewer" : `Show all ${runs.length}`}
          </button>
        )}
      </div>
      <CardGrid>
        {shown.map((run) => (
          <RunCard
            key={run.id}
            run={run}
            grouped
            attention={ATTENTION_STATES.has(run.state as string)}
            done={isTerminalRunState(run.state)}
            onOpen={onOpen}
            onKill={onKill}
          />
        ))}
      </CardGrid>
    </section>
  );
}

function RunCard({
  run,
  attention,
  done,
  grouped,
  onOpen,
  onKill,
}: {
  run: AgentRun;
  attention?: boolean;
  done?: boolean;
  /** This card sits under a shared-title header, so it names its own work
   *  rather than repeating the title — see rowHeadline. */
  grouped?: boolean;
  onOpen: (id: string) => void;
  onKill: (id: string) => void;
}) {
  const terminal = isTerminalRunState(run.state);
  const attachable = !!run.interactive && run.state === "RUNNING";
  const note = attentionNote(run.state);

  // fix: this container used to be role="button" tabIndex={0} — a widget
  // role directly nesting the real Attach/Review/kebab <button>s below,
  // which is an invalid ARIA structure (interactive-in-interactive). Mouse
  // click-to-open stays via the plain onClick; keyboard/AT users already
  // have a dedicated affordance for the same action (RunActions' "Open
  // detail" menu item), so no functionality is lost by dropping the role.
  return (
    <div
      onClick={() => onOpen(run.id)}
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
          {rowHeadline(run, !!grouped)}
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
        <div className="flex items-center gap-1.5 text-[0.7813rem] text-warning">
          <BellRing className="size-3.5 shrink-0" />
          <span>{note}</span>
        </div>
      )}

      <div className="mt-auto flex flex-wrap items-center gap-x-2 gap-y-1.5 border-t border-border pt-3">
        <RunStateBadge state={run.state} />
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
          <Mono className="max-w-[8rem] truncate text-[0.6875rem]" title={run.id}>
            {shortId(run.id)}
          </Mono>
          <span className="whitespace-nowrap text-[0.6875rem] text-muted-foreground" title={run.created_at}>
            {relativeTime(run.created_at)}
          </span>
        </div>
      </div>
    </div>
  );
}

function RunsTable({
  groups,
  loose,
  cap,
  onLoadMore,
  onOpen,
  onKill,
}: {
  groups: { title: string; runs: AgentRun[] }[];
  loose: AgentRun[];
  cap: number;
  onLoadMore: () => void;
  onOpen: (id: string) => void;
  onKill: (id: string) => void;
}) {
  // The same grouping the board shows, flattened into rows with a header row
  // per group so members are adjacent AND labelled. Capping the FLATTENED list
  // (rather than per group) keeps "N of M" meaning what it always did.
  const flat: ({ header: string } | AgentRun)[] = [
    ...groups.flatMap((g) => [{ header: g.title }, ...g.runs]),
    ...loose,
  ];
  const rows = [...groups.flatMap((g) => g.runs), ...loose];
  const groupedIds = new Set(groups.flatMap((g) => g.runs.map((r) => r.id)));
  const shown = flat.slice(0, cap + groups.length);
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
          {shown.map((row) => {
            if ("header" in row) {
              return (
                <TableRow key={`h:${row.header}`} className="hover:bg-transparent">
                  <TableCell colSpan={7} className="bg-surface-2/40 py-1.5">
                    <span className="text-[0.6875rem] font-semibold uppercase tracking-wider text-muted-foreground">
                      {row.header}
                    </span>
                  </TableCell>
                </TableRow>
              );
            }
            const run = row;
            const terminal = isTerminalRunState(run.state);
            const attachable = !!run.interactive && run.state === "RUNNING";
            return (
              // fix: same nested-interactive-widget issue as the board's
              // RunCard (role="button" wrapping the real per-row action
              // buttons) — dropped for the same reason; see RunCard above.
              <TableRow
                key={run.id}
                onClick={() => onOpen(run.id)}
                className="cursor-pointer"
              >
                <TableCell>
                  <div className="flex min-w-0 items-center gap-2.5">
                    <AgentBadge agent={run.agent} withLabel={false} />
                    <span className="block max-w-[320px] truncate text-sm font-medium text-foreground">
                      {rowHeadline(run, groupedIds.has(run.id))}
                    </span>
                  </div>
                </TableCell>
                <TableCell>
                  <RunStateBadge state={run.state} />
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
                  <span className="whitespace-nowrap font-mono text-[0.7188rem] text-muted-foreground">{run.id}</span>
                </TableCell>
                <TableCell>
                  <span className="whitespace-nowrap text-xs text-muted-foreground" title={run.created_at}>
                    {relativeTime(run.created_at)}
                  </span>
                </TableCell>
                <TableCell onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()}>
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
  // identical action on Run Detail — one misclick here killed a run with zero
  // chance to back out. The dialog is rendered as a SIBLING of
  // DropdownMenuContent (not nested inside it), controlled by its own state, so
  // it survives the menu's close/unmount.
  const [confirmId, setConfirmId] = React.useState<string | null>(null);
  return (
    // This guard is RunCard's only defense (the board is the default Mode —
    // RunCard has no TableCell of its own to also carry it, unlike the table
    // row above): onClick-only let Enter/Space on the kebab reach RunCard's
    // row-level onKeyDown and navigate instead of opening the menu.
    <div onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()}>
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
            onClick={() => setConfirmId(run.id)}
            className="text-danger focus:text-danger"
          >
            <Skull className="size-4" /> Kill run
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>

      <KillRunDialog
        runId={confirmId}
        onOpenChange={(o) => !o && setConfirmId(null)}
        onConfirm={() => onKill(run.id)}
      />
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
