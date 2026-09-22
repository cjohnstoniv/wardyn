/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Demo episode rows (episode-card.tsx) — the funnel steps' "Watch" affordance
// and the welcome hero's full catalog. Canon per the approved mock.
export const EPISODES_COPY = {
  WATCH: "Watch",
  CLOSE: "Close",
  NOT_RECORDED: "Not recorded yet",
  STREAM_NOTE: "Streams from the Wardyn release on GitHub only after you press Watch. Nothing is prefetched.",
  LOAD_ERROR: "Couldn't load this episode from GitHub.",
  OPEN_RELEASE_PAGE: "Open the release page",
  ALL_EPISODES_TITLE: "All episodes",
  // Derived from EPISODES (recorded count, summed minutes) — never hand-typed,
  // so a re-shoot that ships/reserves an episode can't leave this stale.
  SUMMARY: (recorded: number, minutes: number) => `${recorded} recorded · about ${minutes} minutes · streamed from GitHub on click`,
  // Shape C grouping (approved mock round 2026-08-31): path-first groups, the
  // install's own deployment leading, the other path collapsed.
  GROUP_CORE: "Start here",
  GROUP_DEPLOYMENT_SINGLE: "Your deployment — single-user",
  GROUP_DEPLOYMENT_MULTI: "Your deployment — multi-user",
  GROUP_ANY: "Running work — any deployment",
  OTHER_PATH_SINGLE: (n: number) => `The single-user path — ${n} episodes`,
  OTHER_PATH_MULTI: (n: number) => `The multi-user path — ${n} episodes`,
  FOR_YOUR_MEMBERS: "For your members",
  MEMBER_YOUR_PATH: "Your path",
} as const;

// DRAFT (M2 canon pending) — X4-F3 (runs-first-run-demos.tsx's "See it work"
// grid subtitle): the prior sentence "No model, no key, no repo" was
// contradicted ten lines below by needsModel/needsSecret — some demo cards
// genuinely require a connected model or a stored secret. Canon row:
// local/v074/canon/docs.md, key runs-first-run-demos.subtitle.
export const FIRST_RUN_DEMOS_SUBTITLE =
  "No repo needed. Most need no model or key either — a few show what a connected model or a stored secret additionally protects. Each one runs a real governed sandbox in about a minute.";

