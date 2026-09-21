/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Link } from "react-router-dom";
import {
  CircleX,
  FilterX,
  Loader2,
  Play,
  Plus,
  RotateCw,
  Search,
  SquareTerminal,
} from "lucide-react";
import type { AgentRun, Recording } from "../../lib/types";
import { runHeadline } from "../../lib/types";
import { recordings as api } from "../../lib/api/recordings";
import { runs as runsApi } from "../../lib/api/runs";
import { useRecordingDisabled } from "../../lib/hooks/use-recording-disabled";
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
import { AgentBadge, ConfinementChip, RunStateBadge } from "../wardyn/primitives";
import { Mono } from "../wardyn/code-block";
import { EmptyState, ErrorState } from "../wardyn/states";
import { RECORDING_DISABLED_DESC, RECORDING_DISABLED_TITLE } from "../wardyn/copy";
import {
  RECORDINGS_ALL_LOADED,
  RECORDINGS_FILTER_SCOPE,
  RECORDINGS_LOADING,
  RECORDINGS_LOAD_MORE,
  RECORDINGS_MORE_NOTE,
  RECORDINGS_PAGE_ERROR_BODY,
  RECORDINGS_PAGE_ERROR_TITLE,
} from "./recording-copy";
import { PageHeader } from "../wardyn/page-header";
import { TerminalPlayer } from "../wardyn/terminal-player";

// #159: the server-side page size. `?limit=&offset=` has been supported by
// /runs for a while; nothing here read it until this screen — the client-side
// cap this replaced only re-sliced a window that listRuns() had already
// fetched (and, past LIST_LIMIT, already dropped rows from). 100 keeps the
// first paint cheap; four presses covers a thousand runs.
const PAGE_SIZE = 100;

// R4-F077: has_recording / recording_bytes / recording_duration_sec are
// DERIVED fields on AgentRun (internal/types.AgentRun; ui/lib/types/runs.ts
// mirrors them), projected server-side from RecordingStore.StatAndTail. This
// screen builds its WHOLE library from the one listRuns() call those fields
// ride on — has_recording filters the library, the other two render straight
// onto a card. It used to ask every run individually (api.probeRecording,
// since removed here) because "has a recording" used to be nothing but "does
// GET .../recording/{id} resolve" — answering that meant downloading every
// run's WHOLE cast just to learn yes/no/how-big/how-long (39.8 MB measured
// for 200 runs). A zero recording_duration_sec does NOT mean "no
// recording" (a header-only cast is a real, zero-length one) — has_recording
// is the only signal for that. The interactive-attach composite key
// (`<run-id>~<session-uuid>`) is still outside this projection; those
// recordings surface on Run Detail instead, from that run's own audit trail.
// A cast is fetched (api.getRecording) only once a viewer presses play.
function formatDuration(totalSeconds: number): string {
  const s = Math.max(0, Math.round(totalSeconds));
  const m = Math.floor(s / 60);
  return `${m}:${String(s % 60).padStart(2, "0")}`;
}

// Only show search/facets once there's enough of a library to make them
// useful — a filter bar over two cards is just noise.
// fixed threshold; make it configurable if it ever matters.
const MIN_CARDS_FOR_FILTERS = 4;

export function RecordingScreen() {
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [runs, setRuns] = React.useState<AgentRun[]>([]);
  // Whether the server told us (X-Wardyn-Truncated) that more rows exist past
  // the pages fetched so far. No total ever comes with it (#159) — this is a
  // yes/no, never a count of what's left.
  const [truncated, setTruncated] = React.useState(false);
  const [loadingMore, setLoadingMore] = React.useState(false);
  // A failed LOAD MORE, not a failed first page (that's `status === "error"`,
  // unchanged below) — the rows already on screen stay put and Retry resumes
  // from the same offset, because the failed fetch never touched `runs`.
  const [pageError, setPageError] = React.useState(false);

  const [query, setQuery] = React.useState("");
  const [agentFacet, setAgentFacet] = React.useState("all");
  const [stateFacet, setStateFacet] = React.useState("all");
  const [playing, setPlaying] = React.useState<AgentRun | null>(null);
  // The ONE cast a viewer pressed play on — fetched (never at load time) by
  // the effect below, which also owns the loading/error state for that fetch.
  const [playingRecording, setPlayingRecording] = React.useState<Recording | null>(null);
  const [playError, setPlayError] = React.useState<string | null>(null);

  // Now the shared hook: the same /healthz read the run cockpit and
  // the New Run rail make. See use-recording-disabled.ts.
  const recordingDisabled = useRecordingDisabled() === true;

  const load = React.useCallback(() => {
    let cancelled = false;
    setStatus("loading");
    setPageError(false);
    runsApi
      .listRuns({ includeRecordingMeta: true, limit: PAGE_SIZE, offset: 0 })
      .then((got) => {
        if (cancelled) return;
        setRuns(got.runs);
        setTruncated(got.truncated);
        setStatus("ready");
      })
      .catch(() => {
        if (!cancelled) setStatus("error");
      });
    return () => {
      cancelled = true;
    };
  }, []);

  React.useEffect(load, [load]);

  // #159: fetch the next PAGE_SIZE rows starting where the loaded set ends.
  // `runs.length` IS the next offset — every row this screen holds came from
  // one of these paged fetches, one page at a time, so nothing else can have
  // advanced it out from under this call.
  const loadMore = React.useCallback(() => {
    setLoadingMore(true);
    setPageError(false);
    runsApi
      .listRuns({ includeRecordingMeta: true, limit: PAGE_SIZE, offset: runs.length })
      .then((got) => {
        setRuns((prev) => [...prev, ...got.runs]);
        setTruncated(got.truncated);
      })
      .catch(() => setPageError(true))
      .finally(() => setLoadingMore(false));
  }, [runs.length]);

  // Fetches the cast for `playing` — and ONLY `playing` — whenever it
  // changes. Nothing here runs while the library is just being browsed.
  React.useEffect(() => {
    if (!playing) {
      setPlayingRecording(null);
      setPlayError(null);
      return;
    }
    let cancelled = false;
    setPlayingRecording(null);
    setPlayError(null);
    api
      .getRecording(playing.id)
      .then((rec) => {
        if (cancelled) return;
        if (!rec) {
          setPlayError("This run's recording could not be loaded.");
          return;
        }
        setPlayingRecording(rec);
      })
      .catch(() => {
        if (!cancelled) setPlayError("This run's recording could not be loaded.");
      });
    return () => {
      cancelled = true;
    };
  }, [playing]);

  const library = React.useMemo(() => runs.filter((r) => r.has_recording), [runs]);

  const agentOptions = React.useMemo(
    () => Array.from(new Set(library.map((r) => r.agent))),
    [library],
  );
  const stateOptions = React.useMemo(
    () => Array.from(new Set(library.map((r) => r.state))),
    [library],
  );

  const q = query.trim().toLowerCase();
  const filtered = library
    .filter((r) => {
      if (agentFacet !== "all" && r.agent !== agentFacet) return false;
      if (stateFacet !== "all" && r.state !== stateFacet) return false;
      if (q) {
        const hay = `${r.title ?? ""} ${r.task} ${r.repo} ${r.id} ${r.agent}`.toLowerCase();
        if (!hay.includes(q)) return false;
      }
      return true;
    })
    .sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime());

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
        description="Captured terminal sessions, replayed byte-for-byte. Built from the run list alone — a run's own cast is fetched only when you press play."
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
      ) : runs.length === 0 ? (
        <div className="rounded-xl border border-dashed border-border">
          <EmptyState
            icon={SquareTerminal}
            title={recordingDisabled ? RECORDING_DISABLED_TITLE : "Recordings appear once a run's terminal session is captured"}
            description={
              recordingDisabled
                ? RECORDING_DISABLED_DESC
                : "When a run's runner supports session capture, its terminal is recorded and its replay appears here. Launch a run to get started."
            }
            action={
              recordingDisabled ? undefined : (
                <Button asChild size="sm">
                  <Link to="/runs">
                    <Plus className="size-4" /> Go to Runs
                  </Link>
                </Button>
              )
            }
          />
        </div>
      ) : library.length === 0 ? (
        <div className="rounded-xl border border-dashed border-border">
          <EmptyState
            icon={SquareTerminal}
            title={recordingDisabled ? RECORDING_DISABLED_TITLE : "None of your runs have a recording yet"}
            description={
              recordingDisabled
                ? RECORDING_DISABLED_DESC
                : "A recording is produced once an agent process runs in the sandbox and its PTY is captured by wardyn-rec."
            }
            action={
              recordingDisabled ? undefined : (
                <Button asChild size="sm">
                  <Link to="/runs">
                    <Plus className="size-4" /> Go to Runs
                  </Link>
                </Button>
              )
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
                        <RunStateBadge state={s} />
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
              <span className="ml-auto text-xs text-muted-foreground">
                Showing {filtered.length} of {library.length} recording{library.length === 1 ? "" : "s"}
                {/* #159: filters only ever ran over the pages fetched so far —
                    an honest caveat only earns its place once there's
                    genuinely more, unfetched, that a filter can't see. */}
                {filtersActive && truncated && <> · {RECORDINGS_FILTER_SCOPE(library.length)}</>}
              </span>
            </div>
          )}

          {filtered.length === 0 ? (
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
          ) : (
            <div className="grid grid-cols-1 gap-3.5 sm:grid-cols-2 lg:grid-cols-3">
              {filtered.map((run) => (
                <RecordingCard key={run.id} run={run} onPlay={() => setPlaying(run)} />
              ))}
            </div>
          )}

          {/* #159 — the house pattern for a paged list: a text link matching
              the Runs board's own "Load N more" (runs.tsx#RunsTable), never a
              second button convention. No total ever renders — see
              recording-copy.ts. */}
          <div className="mt-4">
            {pageError ? (
              <div className="flex items-start gap-2.5 rounded-xl border border-danger/30 bg-danger-subtle p-3.5">
                <CircleX className="mt-0.5 size-4 shrink-0 text-danger" />
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium text-foreground">{RECORDINGS_PAGE_ERROR_TITLE}</p>
                  <p className="mt-0.5 text-xs text-muted-foreground">
                    {RECORDINGS_PAGE_ERROR_BODY(library.length)}
                  </p>
                </div>
                <Button variant="outline" size="sm" onClick={loadMore}>
                  <RotateCw className="size-3.5" /> Retry
                </Button>
              </div>
            ) : truncated ? (
              <p className="text-center text-xs text-muted-foreground">
                {RECORDINGS_MORE_NOTE(library.length)}{" "}
                <button
                  type="button"
                  onClick={loadMore}
                  disabled={loadingMore}
                  className="font-medium text-info hover:underline disabled:pointer-events-none disabled:opacity-60"
                >
                  {loadingMore ? RECORDINGS_LOADING : RECORDINGS_LOAD_MORE(PAGE_SIZE)}
                </button>
              </p>
            ) : (
              <p className="text-center text-xs text-muted-foreground">{RECORDINGS_ALL_LOADED(library.length)}</p>
            )}
          </div>
        </>
      )}

      {playing && (
        <Dialog open onOpenChange={(open) => !open && setPlaying(null)}>
          <DialogContent className="sm:max-w-3xl">
            <DialogHeader>
              <DialogTitle className="truncate pr-6">{runHeadline(playing)}</DialogTitle>
              <DialogDescription className="flex flex-wrap items-center gap-2">
                <AgentBadge agent={playing.agent} />
                <ConfinementChip value={playing.confinement_class} />
                <Mono>{playing.repo}</Mono>
              </DialogDescription>
            </DialogHeader>
            {playError ? (
              <ErrorState message={playError} onRetry={() => setPlaying({ ...playing })} />
            ) : playingRecording ? (
              <TerminalPlayer recording={playingRecording} />
            ) : (
              <div className="flex h-[300px] items-center justify-center">
                <Loader2 className="size-5 animate-spin text-muted-foreground" />
              </div>
            )}
          </DialogContent>
        </Dialog>
      )}
    </div>
  );
}

function RecordingCard({ run, onPlay }: { run: AgentRun; onPlay: () => void }) {
  const durationSec = run.recording_duration_sec;
  const bytes = run.recording_bytes ?? 0;
  return (
    // ui-auditRec-4: the "Open run" link used to nest INSIDE this role=button
    // card (an ARIA nested-interactive anti-pattern — the stopPropagation on
    // both click and keydown was already papering over the collision that
    // caused). Only the thumbnail+task surface below is the button now; the
    // metadata/link row is a separate, non-clickable footer sibling, so
    // there's exactly one focusable target per subtree and the link needs no
    // propagation guard at all (it's no longer a descendant of the button).
    <div className="flex flex-col overflow-hidden rounded-xl border border-border bg-card transition-colors hover:border-border-strong">
      <div
        role="button"
        tabIndex={0}
        onClick={onPlay}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            onPlay();
          }
        }}
        className="flex cursor-pointer flex-col"
      >
        {/* skipped a fake per-card terminal-output preview (the design
            mock used static demo lines) — rendering real captured ANSI output
            safely at thumbnail size needs its own escaping/parsing pass. This
            shows real signals only (icon + measured duration); add a genuine
            text preview later if it earns its complexity. */}
        <div className="relative flex h-24 items-end border-b border-border bg-surface-2/60 px-4 py-3">
          <SquareTerminal className="absolute left-4 top-3.5 size-5 text-border" aria-hidden />
          <span className="pointer-events-none absolute right-3 top-3 inline-flex size-9 items-center justify-center rounded-full border border-primary/40 bg-primary/15 text-primary">
            <Play className="size-4 translate-x-px" />
          </span>
          {durationSec != null && (
            <span className="absolute bottom-2.5 right-3 rounded-md border border-border bg-background/85 px-1.5 py-0.5 font-mono text-meta text-muted-foreground">
              {formatDuration(durationSec)}
            </span>
          )}
        </div>

        <div className="flex flex-1 flex-col gap-2.5 p-3.5 pb-0">
          <div className="flex items-start gap-2.5">
            <div className="min-w-0 flex-1">
              <p className="truncate text-sm font-medium text-foreground" title={run.task || undefined}>
                {runHeadline(run)}
              </p>
              <div className="mt-1 flex min-w-0 items-center gap-1.5 text-xs text-muted-foreground">
                <AgentBadge agent={run.agent} />
                <span>·</span>
                <span className="truncate font-mono">{run.repo}</span>
              </div>
            </div>
          </div>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-2 border-t border-border p-3.5 pt-2.5">
        <ConfinementChip value={run.confinement_class} />
        <RunStateBadge state={run.state} />
        <Mono>{fmtBytes(bytes)}</Mono>
        <span className="text-xs text-muted-foreground" title={run.created_at}>
          {relativeTime(run.created_at)}
        </span>
        <Link
          to={`/runs/${encodeURIComponent(run.id)}`}
          className="ml-auto text-xs font-medium text-primary hover:underline"
        >
          Open run →
        </Link>
      </div>
    </div>
  );
}
