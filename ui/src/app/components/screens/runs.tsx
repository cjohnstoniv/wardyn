/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Unified runs — one screen, two densities:
//   - Board: the live run card board (auto-refreshed every ~3s), grouped by the
//     run's title — runs that share one are the same piece of work.
//   - Table: the same runs and the same groups, dense and horizontally
//     scrollable, with a header row per group.
// Titles are the grouping axis; a state-based triage (Needs-attention / Active
// / Done-by-outcome) survives in the state facet, in the attention-first
// ordering, and in each group header's per-state counts. See titleGroups.
// Every card / row navigates to the addressable /runs/:id detail page.
// "New run" lives in the app shell top bar.
import * as React from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { FilterX, LayoutGrid, RotateCw, Rows3, Search } from "lucide-react";
import { toast } from "sonner";
import type { AgentRun, ApprovalRequest, SetupStatus } from "../../lib/types";
import { isTerminalRunState } from "../../lib/types";
import { runs as api } from "../../lib/api/runs";
import { approvals as approvalsApi } from "../../lib/api/approvals";
import { setup as setupApi } from "../../lib/api/setup";
import { LIST_LIMIT } from "../../lib/api/core";
import { usePoll } from "../../lib/use-poll";
import { usePublishAttention } from "../../lib/attention-context";
import { getErrorMessage, relativeTime } from "../../lib/format";
import { deriveReadiness } from "../../lib/readiness";
import { NoBarrierBanner, RunsFirstRun } from "./runs-first-run";
import { RunsMemberEmpty } from "./runs-member-empty";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { AgentBadge, Chip, ConfinementChip, RunStateBadge } from "../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton, TruncatedNote } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { useRole } from "../wardyn/operator-context";
import { useConsoleMode } from "../wardyn/console-view";
import { cn } from "../ui/utils";
import { BoardSkeleton, CardGrid, RunActions, RunCard, SectionHeading } from "./runs/run-card";
import { TitleGroup } from "./runs/title-group";
import { statusDetailSentence } from "./run-status-detail";
import { AttentionLane } from "./runs/attention-lane";
import {
  approvalSignals,
  needsAttention,
  needsYou,
  rowHeadline,
  titleGroups,
  type RunSignals,
} from "./runs/board-groups";

// Runs is the eager landing route, so an eager wizard import would park the
// whole new-run graph (workspaces + secrets screens and their dialogs) in the
// entry chunk for every operator who never clicks "New run". Fetched on the
// click instead; rollup shares the chunk with the other mount sites.

// Live-board refresh cadence — a live board shouldn't need a manual reload to
// feel alive.
const POLL_MS = 3000;

// How often the first-run checklist / no-barrier blocker re-checks setup status.
const SETUP_POLL_MS = 5000;

// Table display cap (client-side; listRuns returns the full set) + load-more step.
const TABLE_STEP = 25;

type Mode = "board" | "table";
type StateFacet = "all" | "attention" | "active" | "done";

export function RunsScreen() {
  const navigate = useNavigate();
  const publishAttention = usePublishAttention();
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
  // while that blocker is up, so the banner clears on its own once a runner
  // comes back (what the banner's `wardyn setup status` diagnoses), with no
  // manual reload — Re-check just fires it early. On a host with a barrier
  // there is nothing to watch for, and /setup/status is expensive (a full
  // ListRuns plus a shell-out host sweep), so the poll stops rather than
  // running forever on the landing screen of every open tab.
  const [setupStatus, setSetupStatus] = React.useState<SetupStatus | null>(null);
  // getSetupStatus only ever REJECTS on a real 401 (every other failure —
  // 404, 5xx, a dropped socket — degrades to READY_FALLBACK, see setup.ts).
  // wfetch's 401 branch has already routed that rejection through the
  // module-level onUnauthorized handler (App.tsx), which flips the shell to
  // the sign-in screen and unmounts this one — so by the time this settles
  // there is nothing left for the board to do with it. Without this .catch
  // the rejection was unhandled: a lapsed session raised a floating
  // unhandled-rejection right on the landing screen.
  const loadSetupStatus = React.useCallback(
    () => setupApi.getSetupStatus().then(setSetupStatus).catch(() => {}),
    [],
  );
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

  // Whether a run is HELD is not on the run — a held approval parks the sandbox
  // while the state stays RUNNING — so the board joins the PENDING approvals
  // onto the same tick that fetches the runs. One extra call on the poll that
  // was already running, and no second cadence to drift against: a card's glyph
  // and the sidebar's amber badge cannot end up a tick out of step about the
  // same run.
  const [signals, setSignals] = React.useState<RunSignals>(new Map());
  // #160 — whether the approvals fetch behind `signals` has resolved at least
  // once. Before it has, the empty-Map default reads as "nothing is held"
  // rather than "not known yet" — TitleGroup's wait row pins a "Checking…"
  // chip instead of trusting the default until this flips true. Stays true
  // for the life of the screen once it first resolves — a later poll's
  // approvals lagging behind its own runs half by a tick is not a reason to
  // make the whole board flash back to Checking.
  const [signalsResolved, setSignalsResolved] = React.useState(false);
  const fetchRuns = React.useCallback(() => {
    // The approvals half is best-effort: an operator who cannot list
    // approvals still gets a board, just without the held join. Losing the
    // whole run list to it would be much the worse failure — caught here so
    // it never rejects the combined fetch below.
    const pending = approvalsApi.listApprovals("PENDING").catch((): ApprovalRequest[] => []);
    // Started together (both above), but runs resolve the BOARD on their own
    // — the approvals join runs independently once it is back. A combined
    // Promise.all here would hold the whole screen on the loading skeleton
    // until the slower of the two returns, which is exactly the window
    // "Checking…" exists to cover: without this split, `signalsResolved`
    // could never observably be false while a group is actually on screen.
    return api.listRuns().then((r) => {
      setRuns(r);
      setStatus("ready");
      return pending.then((p) => {
        const nextSignals = approvalSignals(p);
        setSignals(nextSignals);
        setSignalsResolved(true);
        // R-1: publish the two nav-badge counts up, off the SAME unfiltered
        // fetch App.tsx's own (paused-while-here) refreshBadges would have
        // used — a no-op when nothing is listening (usePublishAttention's
        // default).
        publishAttention({
          pendingApprovals: p.length,
          attentionCount: r.filter((run) => needsAttention(run, nextSignals)).length,
        });
      });
    });
  }, [publishAttention]);

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
    // New run is its own page, so the openNewRun intent is a redirect —
    // and `replace` keeps Back going where the operator came from rather
    // than bouncing through this screen again.
    navigate("/runs/new", { replace: true });
  }, [location.state, navigate]);

  // Background refresh: update in place, silent on failure (a blip shouldn't
  // blow the board away — keep last-good data and recover next tick).
  // RETURNED so usePoll's in-flight guard has something to wait on: without
  // it a control plane slower than POLL_MS stacked a fresh board fetch on every
  // tick (R4-F074).
  const refresh = React.useCallback(() => {
    return fetchRuns().catch(() => {
      /* keep last-good data */
    });
  }, [fetchRuns]);
  // Nothing pauses the board — New run is its own page, not a dialog mounted
  // here, so nothing on this screen ever needs to pause polling.
  usePoll(refresh, POLL_MS, false);

  // F1-F10: `load` flips status to "loading" and unmounts the whole toolbar
  // (search input, focus and all) for a round trip the board already runs
  // every POLL_MS in the background. A manual refresh takes the background
  // path plus a spinner instead, not a second skeleton.
  const [refreshing, setRefreshing] = React.useState(false);
  const manualRefresh = () => {
    setRefreshing(true);
    refresh().finally(() => setRefreshing(false));
  };

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

  // "Needs attention" is the shared rule in board-groups (held approval, or
  // awaiting confirmation, or failed/killed) — the same predicate App.tsx's
  // sidebar badge counts, rather than this screen's own copy of a state list.
  const attention = filtered.filter((r) => needsAttention(r, signals));
  const active = filtered.filter(
    (r) => !isTerminalRunState(r.state) && !needsAttention(r, signals),
  );
  const done = filtered
    .filter((r) => isTerminalRunState(r.state) && !needsAttention(r, signals))
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
  // The board pins the runs that are ASKING for something into their own lane,
  // so those are grouped OUT of the title groups — a run is in the lane XOR in
  // a group, never rendered twice. The table has no lane, so it groups the
  // whole list. `visible` is already in triage order and filter preserves it,
  // so both splits are stable with no comparator — a poll that changes nothing
  // moves nothing.
  const lane = mode === "board" ? visible.filter((r) => needsYou(r, signals)) : [];
  const { groups: titled, loose } = titleGroups(
    lane.length > 0 ? visible.filter((r) => !needsYou(r, signals)) : visible,
  );

  const openRun = (id: string) => navigate(`/runs/${encodeURIComponent(id)}`);

  const clearFilters = () => {
    setQuery("");
    setStateFacet("all");
    setAgentFacet("all");
    setRepoFacet("all");
  };

  const role = useRole();
  // The description is keyed on the VIEW (M-7, admin-member-modes-design.md
  // §6, RUNS.DESCRIPTION_ADMIN/USER), not the viewer's role: an admin reading
  // /runs in the user view sees the same "Your runs" line a member does — the
  // list itself is already scoped server-side either way (handleListRuns's
  // creator-pager branch), so this is copy only.
  const view = useConsoleMode();
  const description =
    view === "admin" ? "Every run, live — each confined behind its own barrier." : `Your runs · ${runs.length}`;

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

          {/* #215 — "Workspace" everywhere this facet appears (trigger,
              aria-label, "All" option, and the table's column header below):
              the product's own word, not the wire field's ("Repo"). */}
          <Select value={repoFacet} onValueChange={setRepoFacet}>
            <SelectTrigger size="sm" className="w-[170px]" aria-label="Workspace">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">Workspace · All</SelectItem>
              {repos.map((r) => (
                <SelectItem key={r} value={r}>
                  {r}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>

          {/* Same shared primitive Audit uses for the "live" concept — a Chip
              pill. #215: "Live" alone — "refreshes every 3s" narrated the
              polling implementation to a person who does not need to know it;
              the title says the same thing without the number. */}
          <Chip tone="success" dot pulse className="ml-auto" title="Refreshing on its own">
            Live
          </Chip>
          <Button
            variant="outline"
            size="icon"
            onClick={manualRefresh}
            aria-label="Refresh now"
          >
            {/* R-7: NOT disabled={refreshing} — a disabled element isn't
                focusable, so a real browser blurs the just-clicked button to
                <body> for the round trip, the exact focus loss F1-F10 exists
                to fix. A second click just re-enters the same background
                path (refresh() has no in-flight guard of its own to race). */}
            <RotateCw className={cn("size-4", refreshing && "animate-spin")} />
          </Button>
        </div>
      )}

      {/* Past the cap the facets above filter only the fetched window, so a
          "no runs match" would be a lie — say the window is a window. */}
      <TruncatedNote count={runs.length} cap={LIST_LIMIT} />

      {status === "loading" ? (
        // Renders the density-matching skeleton — the board card-grid one
        // only in Board density — so a manual Refresh or re-navigation in
        // Table mode doesn't flash the wrong shape.
        mode === "table" ? (
          <div className="overflow-hidden rounded-xl border border-border bg-card">
            <TableSkeleton rows={8} cols={view === "admin" ? 8 : 7} />
          </div>
        ) : (
          <BoardSkeleton />
        )
      ) : status === "error" ? (
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          <ErrorState
            action={
              <Button variant="outline" size="sm" onClick={load}>
                <RotateCw className="size-3.5" /> Retry
              </Button>
            }
          />
        </div>
      ) : trueEmpty && role !== "admin" ? (
        /* X3-F4: RunsFirstRun is the OPERATOR's funnel — a host-barrier readout
           a member's redacted status renders blank, over steps their role cannot
           reach. Same board, the member's own answer.
           `!== "admin"`, never `=== "member"`. The redaction that makes
           the funnel blank is keyed on isOperator (internal/api/setup.go), which
           is SUPER-admin only — so a security admin's status arrives redacted
           too, and through the two-valued form this tier read every withheld
           field as a fact. Same three-valued shape as setupGateActive and
           GettingStarted, and for the same stated reason. The count line above
           stays `=== "member"`: handleListRuns scopes by creator on
           isSecurityOperator, so a security admin really does see every run. */
        <RunsMemberEmpty />
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
          <AttentionLane runs={lane} signals={signals} onOpen={openRun} onKill={kill} />

          {titled.map((g) => (
            <TitleGroup
              key={g.title}
              title={g.title}
              runs={g.runs}
              signals={signals}
              signalsResolved={signalsResolved}
              open={!!expanded[g.title]}
              onToggle={() => setExpanded((s) => ({ ...s, [g.title]: !s[g.title] }))}
              onOpen={openRun}
              onKill={kill}
            />
          ))}

          {loose.length > 0 && (
            <section aria-label="Other runs">
              {/* Only labelled when there is something to distinguish it FROM —
                  on a board with no shared titles, "Other runs" describes
                  every run on the page and says nothing. #215: "Other runs"
                  replaces "Ungrouped" — a data-model word on a user's screen. */}
              {(titled.length > 0 || lane.length > 0) && (
                <SectionHeading title="Other runs" count={loose.length} />
              )}
              <CardGrid>
                {loose.map((run) => (
                  <RunCard key={run.id} run={run} signals={signals} onOpen={openRun} onKill={kill} />
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

// TitleGroup (one title's runs, plus #160's wait-row) lives in
// runs/title-group.tsx — extracted so this file and its test stay under the
// size gate (scripts/check-file-size.sh) instead of growing past it.

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
  // F1-F7: the old `flat.slice(0, cap + groups.length)` budgeted headers
  // GLOBALLY against the cap, so a header could land exactly on the cut and
  // render as the last row with nothing under it (or the cap could include
  // more than `cap` actual runs). Count data rows while building instead, and
  // drop a trailing header that has none.
  const shown: ({ header: string } | AgentRun)[] = [];
  let shownCount = 0;
  for (const row of flat) {
    if (!("header" in row)) {
      if (shownCount >= cap) break;
      shownCount++;
    }
    shown.push(row);
  }
  if (shown.length > 0 && "header" in shown[shown.length - 1]) shown.pop();
  // M-7: the admin monitor's own descriptor — "every run, with the owner
  // shown" (admin-member-modes-design.md §6) — an extra column rather than a
  // cell folded into an existing one, so it sorts and truncates like every
  // other fact here instead of overloading Run ID's row.
  const view = useConsoleMode();
  const cols = view === "admin" ? 8 : 7;
  return (
    <div className="overflow-hidden rounded-xl border border-border bg-card">
      <Table className="min-w-[960px]">
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>Run</TableHead>
            <TableHead className="w-[180px]">State</TableHead>
            <TableHead className="w-[130px]">Barrier</TableHead>
            <TableHead className="w-[180px]">Workspace</TableHead>
            {view === "admin" && <TableHead className="w-[180px]">Owner</TableHead>}
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
                  <TableCell colSpan={cols} className="bg-surface-2/40 py-1.5">
                    <span className="text-meta font-semibold uppercase tracking-wider text-muted-foreground">
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
              // Same nested-interactive-widget issue as the board's RunCard
              // (role="button" wrapping the real per-row action buttons) —
              // dropped for the same reason; see RunCard above.
              <TableRow
                key={run.id}
                onClick={() => onOpen(run.id)}
                className="cursor-pointer"
              >
                <TableCell>
                  <div className="flex min-w-0 items-center gap-2.5">
                    <AgentBadge agent={run.agent} withLabel={false} />
                    {/* #215 — a real <a href>, not the row's onClick alone:
                        runs.tsx had the same click-handler-on-a-div defect as
                        the board's card (no anchor, no keyboard path). The
                        row's own onClick above still opens it by mouse
                        anywhere else in the row; stopPropagation here just
                        keeps the click from firing twice. */}
                    <Link
                      to={`/runs/${encodeURIComponent(run.id)}`}
                      onClick={(e) => e.stopPropagation()}
                      className="block max-w-[320px] truncate text-sm font-medium text-foreground hover:underline"
                    >
                      {rowHeadline(run, groupedIds.has(run.id))}
                    </Link>
                  </div>
                </TableCell>
                <TableCell>
                  <RunStateBadge state={run.state} />
                  {/* 0.7.6 finding 6: what a STARTING run is waiting ON, under
                      its badge. The full sentence, not the header's short
                      register — the cell is wide and this is the one surface
                      where a person scanning the board can tell a pull from a
                      scheduling failure without opening the run. Absent for
                      every run the server did not send a reason for. */}
                  {statusDetailSentence(run.status_detail, run.status_reason) && (
                    <span className="mt-0.5 block max-w-[260px] truncate text-meta text-muted-foreground">
                      {statusDetailSentence(run.status_detail, run.status_reason)}
                    </span>
                  )}
                </TableCell>
                <TableCell>
                  <ConfinementChip value={run.confinement_class} />
                </TableCell>
                <TableCell>
                  <span className="whitespace-nowrap font-mono text-xs text-muted-foreground">{run.repo}</span>
                </TableCell>
                {view === "admin" && (
                  <TableCell>
                    <span className="block max-w-[170px] truncate text-xs text-muted-foreground" title={run.created_by}>
                      {run.created_by}
                    </span>
                  </TableCell>
                )}
                <TableCell>
                  {/* Run ID never truncates — it stays fully readable and the table
                      scrolls horizontally instead. */}
                  <span className="whitespace-nowrap font-mono text-meta text-muted-foreground">{run.id}</span>
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
              <button onClick={onLoadMore} className="font-medium text-info hover:underline">
                Load {TABLE_STEP} more
              </button>
            </>
          )}
        </span>
      </div>
    </div>
  );
}
