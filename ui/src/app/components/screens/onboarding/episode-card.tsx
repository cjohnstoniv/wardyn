/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Demo episode rows — the funnel steps' inline "Watch" affordance and the
// welcome hero's full catalog. Streaming is opt-in only: no <video> element
// exists in the DOM until the viewer presses Watch — no autoplay, no poster,
// no third-party player, nothing prefetched.
import * as React from "react";
import { Play } from "lucide-react";
// ponytail: lucide's own Play glyph IS the "lucide-style" play icon the brief
// asks for — already a dependency, already imported elsewhere (app-shell.tsx's
// Recordings nav icon) — a hand-drawn inline <svg> would just re-encode the
// same triangle.
import { Button } from "../../ui/button";
import { Chip, SectionLabel } from "../../wardyn/primitives";
import { EPISODES_COPY as T } from "../../wardyn/copy";
import { EPISODES, episodeUrl, episodesFor, releasePageUrl, type Episode } from "../../../lib/demo-videos";

export function EpisodeRow({ episode, chip }: { episode: Episode; chip?: string }) {
  const [open, setOpen] = React.useState(false);
  const [errored, setErrored] = React.useState(false);
  const url = episodeUrl(episode);

  return (
    <div className="flex flex-col gap-2 border-b border-border py-3 last:border-b-0">
      <div className="flex items-center gap-2">
        <Play className="size-4 shrink-0 text-muted-foreground" aria-hidden />
        <span className="min-w-0 flex-1 truncate text-sm text-foreground">{episode.title}</span>
        {chip && <Chip tone="info">{chip}</Chip>}
        {episode.minutes && <span className="shrink-0 text-xs text-muted-foreground">{episode.minutes}</span>}
        {episode.tag === null ? (
          <Chip tone="neutral">{T.NOT_RECORDED}</Chip>
        ) : open ? (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => {
              setOpen(false);
              setErrored(false);
            }}
          >
            {T.CLOSE}
          </Button>
        ) : (
          <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
            {T.WATCH}
          </Button>
        )}
      </div>
      {open && url && !errored && (
        <>
          <video
            controls
            preload="metadata"
            src={url}
            className="w-full rounded-lg"
            onError={() => setErrored(true)}
          />
          <p className="text-xs text-muted-foreground">{T.STREAM_NOTE}</p>
        </>
      )}
      {open && errored && episode.tag !== null && (
        <div className="text-sm text-muted-foreground">
          {T.LOAD_ERROR}{" "}
          <Button variant="link" size="sm" className="h-auto p-0" asChild>
            <a href={releasePageUrl(episode.tag)} target="_blank" rel="noopener noreferrer">
              {T.OPEN_RELEASE_PAGE}
            </a>
          </Button>
        </div>
      )}
    </div>
  );
}

export function StepEpisodes({ stepId }: { stepId: string }) {
  const rows = episodesFor(stepId);
  if (rows.length === 0) return null;
  return (
    <div className="mt-6">
      <SectionLabel>{T.WATCH}</SectionLabel>
      <div className="mt-2">
        {rows.map((e) => (
          <EpisodeRow key={e.id} episode={e} />
        ))}
      </div>
    </div>
  );
}


// Derived from EPISODES itself (never hand-typed) so a re-shoot that ships or
// reserves an episode (RELEASING.md's "one sed") cannot leave the summary
// line stale — see catalogSummary's own test for the arithmetic pin.
export function catalogSummary(episodes: Episode[]): { recorded: number; minutes: number } {
  const recorded = episodes.filter((e) => e.tag !== null).length;
  const totalSeconds = episodes.reduce((sum, e) => {
    if (!e.minutes) return sum;
    const [mm, ss] = e.minutes.split(":").map(Number);
    return sum + mm * 60 + ss;
  }, 0);
  return { recorded, minutes: Math.round(totalSeconds / 60) };
}

// Shape C (approved mock round 2026-08-31): path-first groups. Core leads,
// the install's own deployment path follows, path-agnostic "running work"
// episodes next, and the OTHER deployment's path collapses behind a native
// disclosure — the catalog stays complete without leading anyone down the
// wrong install story. In the multi-user group, member-audience rows carry a
// chip so an admin knows which episodes are for their members, not them.
export function EpisodeList({ mode }: { mode: "single" | "multi" }) {
  const { recorded, minutes } = catalogSummary(EPISODES);
  const byPath = (p: Episode["path"]) => EPISODES.filter((e) => e.path === p);
  const other = byPath(mode === "single" ? "multi" : "single");
  const groups: { key: string; label: string; rows: Episode[]; memberChips?: boolean }[] = [
    { key: "core", label: T.GROUP_CORE, rows: byPath("core") },
    mode === "single"
      ? { key: "single", label: T.GROUP_DEPLOYMENT_SINGLE, rows: byPath("single") }
      : { key: "multi", label: T.GROUP_DEPLOYMENT_MULTI, rows: byPath("multi"), memberChips: true },
    { key: "any", label: T.GROUP_ANY, rows: byPath("any") },
  ];
  return (
    <div className="mt-10">
      <h2 className="text-lg font-semibold text-foreground">{T.ALL_EPISODES_TITLE}</h2>
      <p className="mt-1 text-sm text-muted-foreground">{T.SUMMARY(recorded, minutes)}</p>
      <div className="mt-4 space-y-6">
        {groups.map((g) => (
          <div key={g.key}>
            <SectionLabel>{g.label}</SectionLabel>
            <div className="mt-2">
              {g.rows.map((e) => (
                <EpisodeRow
                  key={e.id}
                  episode={e}
                  chip={g.memberChips && e.audience === "member" ? T.FOR_YOUR_MEMBERS : undefined}
                />
              ))}
            </div>
          </div>
        ))}
        <details>
          <summary className="cursor-pointer text-sm text-muted-foreground">
            {mode === "single" ? T.OTHER_PATH_MULTI(other.length) : T.OTHER_PATH_SINGLE(other.length)}
          </summary>
          <div className="mt-2">
            {other.map((e) => (
              <EpisodeRow key={e.id} episode={e} />
            ))}
          </div>
        </details>
      </div>
    </div>
  );
}
