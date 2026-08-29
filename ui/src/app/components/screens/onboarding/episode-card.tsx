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
import { EPISODES, episodeUrl, episodesFor, releasePageUrl, type Episode } from "../../../lib/demo-videos";

export function EpisodeRow({ episode }: { episode: Episode }) {
  const [open, setOpen] = React.useState(false);
  const [errored, setErrored] = React.useState(false);
  const url = episodeUrl(episode);

  return (
    <div className="flex flex-col gap-2 border-b border-border py-3 last:border-b-0">
      <div className="flex items-center gap-2">
        <Play className="size-4 shrink-0 text-muted-foreground" aria-hidden />
        <span className="min-w-0 flex-1 truncate text-sm text-foreground">{episode.title}</span>
        {episode.minutes && <span className="shrink-0 text-xs text-muted-foreground">{episode.minutes}</span>}
        {episode.tag === null ? (
          <Chip tone="neutral">Not recorded yet</Chip>
        ) : open ? (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => {
              setOpen(false);
              setErrored(false);
            }}
          >
            Close
          </Button>
        ) : (
          <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
            Watch
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
          <p className="text-xs text-muted-foreground">
            Streams from the Wardyn release on GitHub only after you press Watch. Nothing is prefetched.
          </p>
        </>
      )}
      {open && errored && episode.tag !== null && (
        <div className="text-sm text-muted-foreground">
          Couldn&apos;t load this episode from GitHub.{" "}
          <Button variant="link" size="sm" className="h-auto p-0" asChild>
            <a href={releasePageUrl(episode.tag)} target="_blank" rel="noopener noreferrer">
              Open the release page
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
      <SectionLabel>Watch</SectionLabel>
      <div className="mt-2">
        {rows.map((e) => (
          <EpisodeRow key={e.id} episode={e} />
        ))}
      </div>
    </div>
  );
}

const AUDIENCE_GROUPS: { audience: Episode["audience"]; label: string }[] = [
  { audience: "admin", label: "For admins" },
  { audience: "member", label: "For members" },
  { audience: "everyone", label: "For everyone" },
];

export function EpisodeList() {
  return (
    <div className="mt-10">
      <h2 className="text-lg font-semibold text-foreground">All episodes</h2>
      <p className="mt-1 text-sm text-muted-foreground">
        13 recorded · about 72 minutes · streamed from GitHub on click
      </p>
      <div className="mt-4 space-y-6">
        {AUDIENCE_GROUPS.map((g) => {
          const rows = EPISODES.filter((e) => e.audience === g.audience);
          if (rows.length === 0) return null;
          return (
            <div key={g.audience}>
              <SectionLabel>{g.label}</SectionLabel>
              <div className="mt-2">
                {rows.map((e) => (
                  <EpisodeRow key={e.id} episode={e} />
                ))}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
