/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 D2 — the Runs landing page, rewritten (design.md, the owner-approved
// packet at wardyn-archive/mock-08/home-runs-1197-packet.html): one calm
// page, a composer, a filter bar that lives in the URL, and sections ordered
// need-then-time (H-1/H-6) instead of a Board/Table density switch over every
// run at once. The row anatomy, colour/glyph rule and the server's own
// `attention` projection replace this screen's old client-side hold join —
// see runs/runs-model.ts for the one rule that used to be split across this
// file, board-groups.ts and App.tsx's badge.
import * as React from "react";
import { useLocation, useNavigate, useSearchParams } from "react-router-dom";
import { FilterX, Hexagon } from "lucide-react";
import { toast } from "sonner";
import type { AgentRun, SetupStatus } from "../../lib/types";
import { runs as api } from "../../lib/api/runs";
import { setup as setupApi } from "../../lib/api/setup";
import { usePoll } from "../../lib/use-poll";
import { deriveReadiness } from "../../lib/readiness";
import { NoBarrierBanner, RunsFirstRun } from "./runs-first-run";
import { RunsMemberEmpty } from "./runs-member-empty";
import { Button } from "../ui/button";
import { cn } from "../ui/utils";
import { EmptyState, ErrorState } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { useRole } from "../wardyn/operator-context";
import { useConsoleMode } from "../wardyn/console-view";
import { RunsComposer } from "./runs/runs-composer";
import { RunsFilterBar } from "./runs/runs-filter-bar";
import { RunRowList } from "./runs/run-row";
import { AdminOlderLimitsCard } from "./runs/admin-older-limits-card";
import {
  applySavedViewOwner,
  DEFAULT_RUNS_FILTERS,
  parseRunsFilters,
  runsFilterToServerOwner,
  runsFilterToServerStatus,
  runsFiltersAreDefault,
  serializeRunsFilters,
  type RunsFilterState,
} from "./runs/runs-filters";
import { isTopQuiet, nonAttentionRuns, sectionRuns, type RunSections } from "./runs/runs-model";
import { groupRunsBy } from "./runs/runs-groups";
import { BUILTIN_RUNS_VIEWS, loadSavedRunsViews, saveRunsView, type SavedRunsView } from "./runs/runs-saved-views";
import {
  RUNS_AGED_WINDOW_LABEL,
  RUNS_INCLUDE_KILLED,
  RUNS_QUIET,
  RUNS_NO_MATCH_BODY,
  RUNS_SECTION,
  RUNS_SHOW_30,
  runsAgedNote,
  runsViewSaved,
} from "../wardyn/copy/runs-landing";

const POLL_MS = 3000;
const SETUP_POLL_MS = 5000;

export function RunsScreen() {
  const navigate = useNavigate();
  const view = useConsoleMode();
  const adminView = view === "admin";
  const role = useRole();
  const location = useLocation();
  const [searchParams, setSearchParams] = useSearchParams();
  const filters = React.useMemo(() => parseRunsFilters(searchParams), [searchParams]);
  const setFilters = React.useCallback(
    (next: RunsFilterState) => setSearchParams(serializeRunsFilters(next), { replace: true }),
    [setSearchParams],
  );

  const [runsList, setRunsList] = React.useState<AgentRun[]>([]);
  const [hiddenOlder, setHiddenOlder] = React.useState(0);
  const [hiddenKilled, setHiddenKilled] = React.useState(0);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  // whether ANY fetch has ever landed. Only the very first load
  // shows the full-page skeleton (design.md §2.1's "Loading" state); every
  // later one (a filter/search change, a view switch) is a REFETCH, and must
  // not unmount the filter bar under it — see the render below.
  const [loadedOnce, setLoadedOnce] = React.useState(false);
  const [earlierOpen, setEarlierOpen] = React.useState(false);
  // H-8: built-ins first, then whatever this browser has saved. Loaded once
  // (localStorage is synchronous) and refreshed locally on Save — no fetch,
  // no server round trip, per design.md §3.6.
  const [savedViews, setSavedViews] = React.useState<SavedRunsView[]>(() => [
    ...BUILTIN_RUNS_VIEWS,
    ...loadSavedRunsViews(),
  ]);

  // a slow response for an earlier keystroke's request must not
  // land after, and overwrite, a newer one's. Each call gets the next id;
  // only the fetch whose id is still current when it settles is applied.
  const requestIdRef = React.useRef(0);
  const fetchRuns = React.useCallback(() => {
    const id = ++requestIdRef.current;
    return api
      .listRunsFiltered({
        view,
        // Only meaningful in the Admin view (H-4) — the User view forces
        // owner=me server-side regardless, so this never sends it there.
        owner: adminView ? runsFilterToServerOwner(filters.scope) : undefined,
        status: runsFilterToServerStatus(filters.status),
        endedWithin: filters.endedWithin,
        includeKilled: filters.includeKilled,
        workspace: filters.workspace === "all" ? undefined : filters.workspace,
        q: filters.q || undefined,
      })
      .then(
        (res) => {
          if (requestIdRef.current !== id) return; // superseded — ignore
          setRunsList(res.runs);
          setHiddenOlder(res.hiddenOlder);
          setHiddenKilled(res.hiddenKilled);
          setStatus("ready");
          setLoadedOnce(true);
        },
        (err) => {
          if (requestIdRef.current !== id) return; // superseded — ignore too
          throw err; // let load()'s own catch below turn this into "error"
        },
      );
  }, [view, adminView, filters.scope, filters.status, filters.endedWithin, filters.includeKilled, filters.workspace, filters.q]);

  const load = React.useCallback(() => {
    setStatus("loading");
    fetchRuns().catch(() => setStatus("error"));
  }, [fetchRuns]);

  // Reload whenever the filters (i.e. the URL) or the view change, and on
  // every navigation to /runs (location.key changes even same-path) — the
  // shell's "New run" navigates back here after a create.
  React.useEffect(load, [load, location.key]);

  // #10/D14: workspace-detail's "Start a run" CTA lands here with route
  // state instead of a stale pre-seed promise — New run is its own page, so
  // the intent is a redirect. `replace` keeps Back going where the operator
  // came from rather than bouncing through this screen again, and clearing
  // it isn't needed: a fresh navigate() replaces location.state outright.
  React.useEffect(() => {
    const s = location.state as { openNewRun?: boolean } | null;
    if (!s?.openNewRun) return;
    void navigate("/runs/new", { replace: true });
  }, [location.state, navigate]);

  const refresh = React.useCallback(() => fetchRuns().catch(() => {}), [fetchRuns]);
  // The page polls the filtered window instead of a flat 1000-row read
  // (design.md §4) — no manual-refresh control or "Live" chip; polling alone
  // carries the "alive" fact now.
  usePoll(refresh, POLL_MS, status !== "ready");

  // Setup status backs the noBarrier banner and RunsFirstRun's own content
  // (readiness/confinementClasses/secretNames) — NOT the true-first-run
  // decision itself. `has_runs` is a bare deployment-wide existence check
  // (internal/api/setup.go), so it is the wrong signal for a member, or an
  // admin's own User view, on a deployment that has OTHER people's runs; see
  // `trueEmpty` below, which reads this caller's own scoped fetch instead.
  const [setupStatus, setSetupStatus] = React.useState<SetupStatus | null>(null);
  const loadSetupStatus = React.useCallback(
    () => setupApi.getSetupStatus().then(setSetupStatus).catch(() => {}),
    [],
  );
  React.useEffect(() => {
    void loadSetupStatus();
  }, [loadSetupStatus]);
  const readiness = setupStatus ? deriveReadiness(setupStatus) : null;
  const confinementClasses = setupStatus?.runner?.confinement_classes ?? [];
  const noBarrier = !!setupStatus && !setupStatus.unreachable && confinementClasses.length === 0;
  usePoll(loadSetupStatus, SETUP_POLL_MS, !noBarrier && !setupStatus?.unreachable);

  const sections: RunSections = React.useMemo(() => sectionRuns(runsList), [runsList]);
  const workspaceOptions = React.useMemo(
    () => Array.from(new Set(runsList.map((r) => r.repo).filter(Boolean))).sort(),
    [runsList],
  );

  // scope only ever narrows anything in the Admin view (H-4) — a stray
  // ?owner=me surviving a view switch must not read as an active filter in
  // the User view, which ignores it entirely (fetchRuns above never sends it
  // there either).
  const effectiveFilters = adminView ? filters : { ...filters, scope: "all" as const };
  const activeFilter =
    filters.q !== "" ||
    filters.status !== "all" ||
    filters.workspace !== "all" ||
    effectiveFilters.scope !== "all";
  const nothingHidden = hiddenOlder + hiddenKilled === 0;
  // The true first-run state: this caller's OWN scoped fetch (server-scoped
  // by view/owner, unlike setup/status's has_runs, which is a bare
  // deployment-wide existence check — internal/api/setup.go's own doc — and
  // so is wrong for a member, or an admin's User view, on a deployment that
  // has OTHER people's history) found nothing, under the DEFAULT filters,
  // with nothing hidden by the window either. Only reachable with default
  // filters, so a deliberate filter that matches nothing still reads as
  // "no match", never as first-run.
  const trueEmpty =
    status === "ready" && runsList.length === 0 && nothingHidden && runsFiltersAreDefault(effectiveFilters);
  // the full-page skeleton is for the FIRST load only. A later
  // "loading" (a filter/search change re-running fetchRuns) is a refetch —
  // the filter bar (and its focused input) stays mounted; `refetching` below
  // swaps in a rows-only skeleton under it instead.
  const firstLoad = status === "loading" && !loadedOnce;
  const refetching = status === "loading" && loadedOnce;
  const noMatch = status === "ready" && !trueEmpty && runsList.length === 0 && (nothingHidden || activeFilter);
  const quiet =
    status === "ready" && !trueEmpty && !noMatch && isTopQuiet(sections) && runsFiltersAreDefault(effectiveFilters);

  // N counts every run the
  // caller owns, BEFORE filters and ageing (home-runs-1197-packet.html:672,
  // S.RUNS_DESC_USER(sc.length)) — not just the rows this fetch happens to
  // show. Under the default filters the two hidden counts (the ageing
  // window, killed-exclusion) are exactly the gap between the shown rows and
  // the true total, so adding them back in matches the mock's own worked
  // example (13 shown + 4 older + 1 killed = "Your runs · 18").
  const description = adminView
    ? "Every run, live — each confined behind its own barrier."
    : `Your runs · ${runsList.length + hiddenOlder + hiddenKilled}`;

  const clearFilters = () => setFilters(DEFAULT_RUNS_FILTERS);

  return (
    <div className="mx-auto max-w-[1400px] px-6 py-6">
      {noBarrier && <NoBarrierBanner onRecheck={loadSetupStatus} />}

      <PageHeader title="Runs" description={description} />

      {adminView && <AdminOlderLimitsCard />}

      {!adminView && <RunsComposer />}

      {status === "error" ? (
        <div className="mt-4 overflow-hidden rounded-xl border border-border bg-card">
          <ErrorState onRetry={load} />
        </div>
      ) : firstLoad ? (
        <RunsLoadingSkeleton />
      ) : trueEmpty ? (
        adminView ? (
          <div className="mt-4 overflow-hidden rounded-xl border border-border bg-card">
            <EmptyState icon={Hexagon} title="No runs yet" />
          </div>
        ) : role !== "admin" ? (
          <RunsMemberEmpty />
        ) : (
          <RunsFirstRun
            readiness={readiness}
            confinementClasses={confinementClasses}
            secretNames={setupStatus?.secrets.present ?? []}
            onNewRun={() => navigate("/runs/new")}
          />
        )
      ) : (
        <>
          <RunsFilterBar
            filters={filters}
            workspaces={workspaceOptions}
            adminView={adminView}
            currentSearch={searchParams.toString()}
            savedViews={savedViews}
            onChange={setFilters}
            onSelectView={(search) =>
              setSearchParams(applySavedViewOwner(search, adminView, filters.scope), { replace: true })
            }
            onSaveView={(name) => {
              setSavedViews([...BUILTIN_RUNS_VIEWS, ...saveRunsView(name, searchParams.toString())]);
              toast(runsViewSaved(name));
            }}
          />

          {refetching ? (
            <RunsLoadingSkeleton />
          ) : noMatch ? (
            <div className="mt-4 overflow-hidden rounded-xl border border-border bg-card">
              <EmptyState
                icon={FilterX}
                title="No runs match these filters."
                description={RUNS_NO_MATCH_BODY}
                action={
                  <Button variant="outline" onClick={clearFilters}>
                    Clear filters
                  </Button>
                }
              />
            </div>
          ) : (
            <div className="mt-4 space-y-5">
              {quiet && <p className="rounded-xl border border-dashed border-border-strong px-3.5 py-2.5 text-sm text-muted-foreground">{RUNS_QUIET}</p>}

              <RunsSection title={adminView ? RUNS_SECTION.NEEDS_ADMIN : RUNS_SECTION.NEEDS} runs={sections.decide} />
              {/* H-3, Admin view only: owners' sign-ins and lost runs — the
                  server never projects by=owner outside the Admin view, so
                  this bucket is empty (and the section a no-op) in the User
                  view regardless of this adminView gate. */}
              {adminView && <RunsSection title={RUNS_SECTION.OWNER} runs={sections.waitingOwner} />}
              {filters.group === "sections" ? (
                <>
                  <RunsSection title={RUNS_SECTION.RUNNING} runs={sections.running} />
                  <RunsSection title={RUNS_SECTION.ENDED_TODAY} runs={sections.endedToday} />
                  <RunsCollapsibleSection
                    title={RUNS_SECTION.EARLIER}
                    runs={sections.earlier}
                    open={earlierOpen}
                    onToggle={() => setEarlierOpen((o) => !o)}
                  />
                  {sections.older.map((bucket) => (
                    <RunsSection key={bucket.label} title={bucket.label} runs={bucket.runs} />
                  ))}
                </>
              ) : (
                // H-6: Group by Workspace/Title replaces the time sections
                // with one section per group — Needs you / Waiting on the
                // owner above still show their own fixed sections either way.
                groupRunsBy(nonAttentionRuns(runsList), filters.group).map((g) => (
                  <RunsSection key={g.label} title={g.label} runs={g.runs} />
                ))
              )}

              <RunsAgedNote
                older={hiddenOlder}
                killed={hiddenKilled}
                endedWithin={filters.endedWithin}
                onShow30={() => setFilters({ ...filters, endedWithin: "30d" })}
                onIncludeKilled={() => setFilters({ ...filters, includeKilled: true })}
              />
            </div>
          )}
        </>
      )}
    </div>
  );
}

function RunsSection({ title, runs }: { title: string; runs: AgentRun[] }) {
  // React's own id, not a slug of `title` — Group by Workspace/Title (H-6)
  // feeds arbitrary, caller-typed labels through here, and two different
  // titles can slugify to the same string (or, for a non-Latin title, to the
  // same empty one), which duplicated this section's own DOM id and left
  // `aria-labelledby` resolving to the WRONG heading.
  const id = React.useId();
  if (runs.length === 0) return null;
  return (
    <section aria-labelledby={id}>
      <div className="mb-2 flex items-center gap-2">
        {/* Axe heading-order: PageHeader's title is the page's
            one h1 — a section heading directly under it must be h2, not h3. */}
        <h2 id={id} className="label-eyebrow">
          {title}
        </h2>
        <span className="rounded-full bg-muted px-1.5 text-meta font-semibold text-muted-foreground">
          {runs.length}
        </span>
      </div>
      <RunRowList runs={runs} />
    </section>
  );
}

function RunsCollapsibleSection({
  title,
  runs,
  open,
  onToggle,
}: {
  title: string;
  runs: AgentRun[];
  open: boolean;
  onToggle: () => void;
}) {
  const id = React.useId();
  if (runs.length === 0) return null;
  return (
    <section aria-labelledby={id}>
      <button
        type="button"
        id={id}
        aria-expanded={open}
        onClick={onToggle}
        className="mb-2 flex items-center gap-2 label-eyebrow"
      >
        {/* The mock's own caret (home-runs-1197-packet.html:585), rotated
            open. */}
        <span
          aria-hidden="true"
          className={cn("text-[10px] transition-transform", open && "rotate-90")}
        >
          ▶
        </span>
        {title}
        <span className="rounded-full bg-muted px-1.5 text-meta font-semibold text-muted-foreground">
          {runs.length}
        </span>
      </button>
      {open && <RunRowList runs={runs} />}
    </section>
  );
}

function RunsAgedNote({
  older,
  killed,
  endedWithin,
  onShow30,
  onIncludeKilled,
}: {
  older: number;
  killed: number;
  endedWithin: string;
  onShow30: () => void;
  onIncludeKilled: () => void;
}) {
  if (older + killed === 0) return null;
  const windowLabel = RUNS_AGED_WINDOW_LABEL[endedWithin] ?? endedWithin;
  return (
    <p className="text-meta text-muted-foreground">
      {runsAgedNote(older, killed, windowLabel)}{" "}
      {older > 0 && endedWithin !== "30d" && endedWithin !== "all" && (
        <button type="button" onClick={onShow30} className="text-info underline underline-offset-4">
          {RUNS_SHOW_30}
        </button>
      )}
      {older > 0 && killed > 0 && endedWithin !== "30d" && endedWithin !== "all" && " · "}
      {killed > 0 && (
        <button type="button" onClick={onIncludeKilled} className="text-info underline underline-offset-4">
          {RUNS_INCLUDE_KILLED}
        </button>
      )}
    </p>
  );
}

// design.md §2.1: "skeleton: 2 section bars and 5 rows, aria-busy".
function RunsLoadingSkeleton() {
  return (
    <div aria-busy="true" aria-label="Loading runs" className="mt-4 space-y-5">
      {[2, 3].map((rowCount, i) => (
        <div key={i}>
          <div className="mb-2 h-3 w-24 animate-pulse rounded bg-muted" />
          <div className="overflow-hidden rounded-xl border border-border bg-card">
            {Array.from({ length: rowCount }).map((_, r) => (
              <div key={r} className="flex items-center gap-3 border-t border-border px-3 py-3 first:border-t-0">
                <div className="size-3.5 shrink-0 animate-pulse rounded-full bg-muted" />
                <div className="h-3.5 flex-1 animate-pulse rounded bg-muted" />
                <div className="h-3.5 w-16 animate-pulse rounded bg-muted" />
              </div>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}
