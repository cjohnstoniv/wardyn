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
import type { AgentRun, SetupStatus } from "../../lib/types";
import { runs as api } from "../../lib/api/runs";
import { setup as setupApi } from "../../lib/api/setup";
import { usePoll } from "../../lib/use-poll";
import { deriveReadiness } from "../../lib/readiness";
import { NoBarrierBanner, RunsFirstRun } from "./runs-first-run";
import { RunsMemberEmpty } from "./runs-member-empty";
import { Button } from "../ui/button";
import { EmptyState, ErrorState } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { useRole } from "../wardyn/operator-context";
import { useConsoleMode } from "../wardyn/console-view";
import { RunsComposer } from "./runs/runs-composer";
import { RunsFilterBar } from "./runs/runs-filter-bar";
import { RunRowList } from "./runs/run-row";
import {
  DEFAULT_RUNS_FILTERS,
  parseRunsFilters,
  runsFilterToServerStatus,
  runsFiltersAreDefault,
  serializeRunsFilters,
  type RunsFilterState,
} from "./runs/runs-filters";
import { isTopQuiet, sectionRuns, type RunSections } from "./runs/runs-model";
import {
  RUNS_AGED_WINDOW_LABEL,
  RUNS_INCLUDE_KILLED,
  RUNS_QUIET,
  RUNS_NO_MATCH_BODY,
  RUNS_SECTION,
  RUNS_SHOW_30,
  runsAgedNote,
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
  const [earlierOpen, setEarlierOpen] = React.useState(false);

  const fetchRuns = React.useCallback(() => {
    return api
      .listRunsFiltered({
        view,
        status: runsFilterToServerStatus(filters.status),
        endedWithin: filters.endedWithin,
        includeKilled: filters.includeKilled,
        workspace: filters.workspace === "all" ? undefined : filters.workspace,
        q: filters.q || undefined,
      })
      .then((res) => {
        setRunsList(res.runs);
        setHiddenOlder(res.hiddenOlder);
        setHiddenKilled(res.hiddenKilled);
        setStatus("ready");
      });
  }, [view, filters.status, filters.endedWithin, filters.includeKilled, filters.workspace, filters.q]);

  const load = React.useCallback(() => {
    setStatus("loading");
    fetchRuns().catch(() => setStatus("error"));
  }, [fetchRuns]);

  // Reload whenever the filters (i.e. the URL) or the view change, and on
  // every navigation to /runs (location.key changes even same-path) — the
  // shell's "New run" navigates back here after a create.
  React.useEffect(load, [load, location.key]);

  const refresh = React.useCallback(() => fetchRuns().catch(() => {}), [fetchRuns]);
  // The page polls the filtered window instead of a flat 1000-row read
  // (design.md §4) — no manual-refresh control or "Live" chip; polling alone
  // carries the "alive" fact now.
  usePoll(refresh, POLL_MS, status !== "ready");

  // Setup status backs the first-run decision (has_runs, scoped by the
  // server to the caller — unlike this screen's own windowed fetch, so it is
  // the one honest signal for "does this person have ANY run at all", not
  // "does the current 7-day window have one").
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

  const activeFilter = filters.q !== "" || filters.status !== "all" || filters.workspace !== "all";
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
    status === "ready" && runsList.length === 0 && nothingHidden && runsFiltersAreDefault(filters);
  const stillLoading = status === "loading";
  const noMatch = status === "ready" && !trueEmpty && runsList.length === 0 && (nothingHidden || activeFilter);
  const quiet =
    status === "ready" && !trueEmpty && !noMatch && isTopQuiet(sections) && runsFiltersAreDefault(filters);

  const description = adminView
    ? "Every run, live — each confined behind its own barrier."
    : `Your runs · ${runsList.length}`;

  const clearFilters = () => setFilters(DEFAULT_RUNS_FILTERS);

  return (
    <div className="mx-auto max-w-[1400px] px-6 py-6">
      {noBarrier && <NoBarrierBanner onRecheck={loadSetupStatus} />}

      <PageHeader title="Runs" description={description} />

      {!adminView && <RunsComposer />}

      {status === "error" ? (
        <div className="mt-4 overflow-hidden rounded-xl border border-border bg-card">
          <ErrorState onRetry={load} />
        </div>
      ) : stillLoading ? (
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
          <RunsFilterBar filters={filters} workspaces={workspaceOptions} onChange={setFilters} />

          {noMatch ? (
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
  if (runs.length === 0) return null;
  const id = `runs-sec-${title.replace(/\W+/g, "-").toLowerCase()}`;
  return (
    <section aria-labelledby={id}>
      <div className="mb-2 flex items-center gap-2">
        <h3 id={id} className="label-eyebrow">
          {title}
        </h3>
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
  if (runs.length === 0) return null;
  const id = `runs-sec-${title.replace(/\W+/g, "-").toLowerCase()}`;
  return (
    <section aria-labelledby={id}>
      <button
        type="button"
        id={id}
        aria-expanded={open}
        onClick={onToggle}
        className="mb-2 flex items-center gap-2 label-eyebrow"
      >
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
