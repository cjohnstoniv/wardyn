/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Link } from "react-router-dom";
import {
  FilterX,
  Loader2,
  Play,
  Plus,
  RotateCw,
  Search,
  SquareTerminal,
} from "lucide-react";
import type { AgentRun, Recording } from "../../lib/types";
import { api } from "../../lib/api";
import { fmtBytes, relativeTime } from "../../lib/format";
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
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import { AgentBadge, ConfinementChip } from "../wardyn/primitives";
import { RunStatusBadge } from "../wardyn/run-status-badge";
import { Mono } from "../wardyn/code-block";
import { EmptyState, ErrorState } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { TerminalPlayer } from "../wardyn/terminal-player";

// The backend has no "list all recordings" endpoint — a recording only exists
// at GET /runs/{id}/recording/{id} (see internal/api/server.go:349-351, which
// mounts the recording sub-router under /runs/{id}/recording and looks the
// cast up by that SAME id again; internal/recording/handler.go's {runID} param
// really is the run id, there's no separate recording id). So "has a
// recording" is NOT a field on AgentRun — the only honest signal is whether
// api.getRecording(run.id) actually resolves with one. We synthesize the
// library client-side: list every run, then check each one.
interface RecordedRun {
  run: AgentRun;
  recording: Recording;
  /** Real elapsed seconds, taken from the last captured event — never fabricated. */
  durationSec?: number;
  /** Real byte size of the fetched cast payload (Blob), not a backend field. */
  bytes: number;
}

function formatDuration(totalSeconds: number): string {
  const s = Math.max(0, Math.round(totalSeconds));
  const m = Math.floor(s / 60);
  return `${m}:${String(s % 60).padStart(2, "0")}`;
}

// Only show search/facets once there's enough of a library to make them
// useful — a filter bar over two cards is just noise.
// ponytail: fixed threshold; make it configurable if it ever matters.
const MIN_CARDS_FOR_FILTERS = 4;

export function RecordingScreen() {
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [totalRuns, setTotalRuns] = React.useState(0);
  const [checkedCount, setCheckedCount] = React.useState(0);
  const [checkFailures, setCheckFailures] = React.useState(0);
  const [library, setLibrary] = React.useState<RecordedRun[]>([]);

  const [query, setQuery] = React.useState("");
  const [agentFacet, setAgentFacet] = React.useState("all");
  const [stateFacet, setStateFacet] = React.useState("all");
  const [playing, setPlaying] = React.useState<RecordedRun | null>(null);
  // Holds the in-flight load's cancel fn so a manual Refresh (which calls load()
  // directly, not via the effect) cancels the PREVIOUS load first. Without this
  // the prior load's per-run getRecording resolutions keep appending into the
  // freshly-reset library → duplicate cards and duplicate React keys.
  const cancelPrev = React.useRef<(() => void) | null>(null);

  const load = React.useCallback(() => {
    cancelPrev.current?.();
    let cancelled = false;
    setStatus("loading");
    setLibrary([]);
    setTotalRuns(0);
    setCheckedCount(0);
    setCheckFailures(0);

    api
      .listRuns()
      .then((runs) => {
        if (cancelled) return;
        setStatus("ready");
        setTotalRuns(runs.length);
        // ponytail: fires one getRecording() per run with no concurrency cap
        // or pagination — fine for a single-operator console's run list; add
        // a queue/limit if that list ever grows into the thousands.
        runs.forEach((run) => {
          api
            .getRecording(run.id)
            .then((rec) => {
              if (cancelled) return;
              if (rec) {
                const durationSec = rec.events.length
                  ? rec.events[rec.events.length - 1][0]
                  : undefined;
                const bytes = new Blob([rec.cast]).size;
                setLibrary((prev) => [...prev, { run, recording: rec, durationSec, bytes }]);
              }
            })
            .catch(() => {
              if (!cancelled) setCheckFailures((f) => f + 1);
            })
            .finally(() => {
              if (!cancelled) setCheckedCount((c) => c + 1);
            });
        });
      })
      .catch(() => {
        if (!cancelled) setStatus("error");
      });

    const cancel = () => {
      cancelled = true;
    };
    cancelPrev.current = cancel;
    return cancel;
  }, []);

  React.useEffect(load, [load]);

  const allChecked = totalRuns > 0 && checkedCount >= totalRuns;

  const agentOptions = React.useMemo(
    () => Array.from(new Set(library.map((e) => e.run.agent))),
    [library],
  );
  const stateOptions = React.useMemo(
    () => Array.from(new Set(library.map((e) => e.run.state))),
    [library],
  );

  const q = query.trim().toLowerCase();
  const filtered = library
    .filter((e) => {
      if (agentFacet !== "all" && e.run.agent !== agentFacet) return false;
      if (stateFacet !== "all" && e.run.state !== stateFacet) return false;
      if (q) {
        const hay = `${e.run.task} ${e.run.repo} ${e.run.id} ${e.run.agent}`.toLowerCase();
        if (!hay.includes(q)) return false;
      }
      return true;
    })
    .sort((a, b) => new Date(b.run.created_at).getTime() - new Date(a.run.created_at).getTime());

  const clearFilters = () => {
    setQuery("");
    setAgentFacet("all");
    setStateFacet("all");
  };
  const filtersActive = query !== "" || agentFacet !== "all" || stateFacet !== "all";
  const showFilters = library.length > MIN_CARDS_FOR_FILTERS || filtersActive;

  return (
    <div className="mx-auto max-w-[1200px] px-6 py-6">
      <PageHeader
        title="Recordings"
        description="Captured terminal sessions, replayed byte-for-byte. There's no separate recordings store — this library is built by checking each run for one, so it's only as complete as that check."
        actions={
          <Button variant="outline" size="sm" onClick={load}>
            <RotateCw className="size-3.5" /> Refresh
          </Button>
        }
      />

      {status === "error" ? (
        <div className="rounded-xl border border-border bg-card">
          <ErrorState message="Couldn't load the list of runs." onRetry={load} />
        </div>
      ) : status === "loading" ? (
        <div className="flex h-[300px] items-center justify-center rounded-xl border border-border bg-card">
          <Loader2 className="size-5 animate-spin text-muted-foreground" />
        </div>
      ) : totalRuns === 0 ? (
        <div className="rounded-xl border border-dashed border-border">
          <EmptyState
            icon={SquareTerminal}
            title="Recordings appear once a run's terminal session is captured"
            description="When a run's runner supports session capture, its terminal is recorded and its replay appears here. Launch a run to get started."
            action={
              <Button asChild size="sm">
                <Link to="/runs">
                  <Plus className="size-4" /> Go to Runs
                </Link>
              </Button>
            }
          />
        </div>
      ) : (
        <>
          {showFilters && (
            <div className="mb-5 flex flex-wrap items-center gap-2.5">
              <div className="relative w-full max-w-xs">
                <Search className="absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input
                  placeholder="Search tasks, repos, run IDs…"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  className="pl-9"
                />
              </div>
              {agentOptions.length > 1 && (
                <Select value={agentFacet} onValueChange={setAgentFacet}>
                  <SelectTrigger className="w-[170px]">
                    <SelectValue placeholder="Agent · All" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">Agent · All</SelectItem>
                    {agentOptions.map((a) => (
                      <SelectItem key={a} value={a}>
                        <AgentBadge agent={a} />
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
              {stateOptions.length > 1 && (
                <Select value={stateFacet} onValueChange={setStateFacet}>
                  <SelectTrigger className="w-[170px]">
                    <SelectValue placeholder="Outcome · All" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">Outcome · All</SelectItem>
                    {stateOptions.map((s) => (
                      <SelectItem key={s} value={s}>
                        <RunStatusBadge state={s} />
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
              <span className="ml-auto text-xs text-muted-foreground">
                {filtered.length} of {library.length} recording{library.length === 1 ? "" : "s"}
                {!allChecked && " so far"}
              </span>
            </div>
          )}

          {filtered.length === 0 && library.length > 0 ? (
            <div className="rounded-xl border border-dashed border-border">
              <EmptyState
                icon={FilterX}
                title="No recordings match these filters"
                action={
                  <Button variant="outline" size="sm" onClick={clearFilters}>
                    Clear filters
                  </Button>
                }
              />
            </div>
          ) : filtered.length === 0 && !allChecked ? (
            <div className="flex h-[200px] items-center justify-center gap-2 rounded-xl border border-border bg-card text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" /> Checking {totalRuns} run
              {totalRuns === 1 ? "" : "s"} for recordings…
            </div>
          ) : filtered.length === 0 ? (
            <div className="rounded-xl border border-dashed border-border">
              <EmptyState
                icon={SquareTerminal}
                title="None of your runs have a recording yet"
                description="A recording is produced once an agent process runs in the sandbox and its PTY is captured by wardyn-rec."
                action={
                  <Button asChild size="sm">
                    <Link to="/runs">
                      <Plus className="size-4" /> Go to Runs
                    </Link>
                  </Button>
                }
              />
            </div>
          ) : (
            <div className="grid grid-cols-1 gap-3.5 sm:grid-cols-2 lg:grid-cols-3">
              {filtered.map((entry) => (
                <RecordingCard key={entry.run.id} entry={entry} onPlay={() => setPlaying(entry)} />
              ))}
            </div>
          )}

          {!allChecked && library.length > 0 && (
            <p className="mt-4 flex items-center gap-2 text-xs text-muted-foreground">
              <Loader2 className="size-3 animate-spin" />
              Checking {Math.max(totalRuns - checkedCount, 0)} more run
              {totalRuns - checkedCount === 1 ? "" : "s"} for a recording…
            </p>
          )}
          {allChecked && checkFailures > 0 && (
            <div className="mt-4 flex items-center gap-2 text-xs text-muted-foreground">
              <span>
                {checkFailures} run{checkFailures === 1 ? "" : "s"} couldn't be checked for a
                recording.
              </span>
              <Button variant="outline" size="sm" onClick={load}>
                <RotateCw className="size-3.5" /> Retry
              </Button>
            </div>
          )}
        </>
      )}

      {playing && (
        <Dialog open onOpenChange={(open) => !open && setPlaying(null)}>
          <DialogContent className="sm:max-w-3xl">
            <DialogHeader>
              <DialogTitle className="truncate pr-6">{playing.run.task}</DialogTitle>
              <DialogDescription className="flex flex-wrap items-center gap-2">
                <AgentBadge agent={playing.run.agent} />
                <ConfinementChip value={playing.run.confinement_class} />
                <Mono>{playing.run.repo}</Mono>
              </DialogDescription>
            </DialogHeader>
            <TerminalPlayer recording={playing.recording} />
          </DialogContent>
        </Dialog>
      )}
    </div>
  );
}

function RecordingCard({ entry, onPlay }: { entry: RecordedRun; onPlay: () => void }) {
  const { run, durationSec, bytes } = entry;
  return (
    <div
      onClick={onPlay}
      className="flex cursor-pointer flex-col overflow-hidden rounded-xl border border-border bg-card transition-colors hover:border-border-strong"
    >
      {/* ponytail: skipped a fake per-card terminal-output preview (the design
          mock used static demo lines) — rendering real captured ANSI output
          safely at thumbnail size needs its own escaping/parsing pass. This
          shows real signals only (icon + measured duration); add a genuine
          text preview later if it earns its complexity. */}
      <div className="relative flex h-24 items-end border-b border-border bg-surface-2/60 px-4 py-3">
        <SquareTerminal className="absolute left-4 top-3.5 size-5 text-muted-foreground/40" />
        <span className="pointer-events-none absolute right-3 top-3 inline-flex size-9 items-center justify-center rounded-full border border-primary/40 bg-primary/15 text-primary">
          <Play className="size-4 translate-x-px" />
        </span>
        {durationSec != null && (
          <span className="absolute bottom-2.5 right-3 rounded-md border border-border bg-background/85 px-1.5 py-0.5 font-mono text-[11px] text-muted-foreground">
            {formatDuration(durationSec)}
          </span>
        )}
      </div>

      <div className="flex flex-1 flex-col gap-2.5 p-3.5">
        <div className="flex items-start gap-2.5">
          <div className="min-w-0 flex-1">
            <p className="truncate text-sm font-medium text-foreground" title={run.task}>
              {run.task}
            </p>
            <div className="mt-1 flex min-w-0 items-center gap-1.5 text-xs text-muted-foreground">
              <AgentBadge agent={run.agent} />
              <span>·</span>
              <span className="truncate font-mono">{run.repo}</span>
            </div>
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-2 border-t border-border pt-2.5">
          <ConfinementChip value={run.confinement_class} />
          <RunStatusBadge state={run.state} />
          <Mono>{fmtBytes(bytes)}</Mono>
          <span className="text-xs text-muted-foreground" title={run.created_at}>
            {relativeTime(run.created_at)}
          </span>
          <Link
            to={`/runs/${encodeURIComponent(run.id)}`}
            onClick={(e) => e.stopPropagation()}
            className="ml-auto text-xs font-medium text-primary hover:underline"
          >
            Open run →
          </Link>
        </div>
      </div>
    </div>
  );
}
