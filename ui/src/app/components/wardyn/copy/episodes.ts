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
  // Reused (canon docs/design/demo-video-source-canon.md): the default,
  // unconfigured deployment — episodes stream from the Wardyn GitHub release.
  STREAM_NOTE: "Streams from the Wardyn release on GitHub only after you press Watch. Nothing is prefetched.",
  // New: an operator-configured mirror (WARDYN_DEMO_VIDEO_BASE_URL) replaces
  // "GitHub" with "your admin configured" — the host/URL itself never renders
  // (Q145-2), so this can't leak an internal mirror address into the DOM.
  STREAM_NOTE_CONFIGURED:
    "Streams from the video source your admin configured, only after you press Watch. Nothing is prefetched.",
  // Reused, default source only. A redirecting mirror (or a GitHub host move)
  // fails CLOSED at the CSP's media-src with no server error at all — the
  // browser just refuses the <video> load, which reads as a missing file.
  LOAD_ERROR: "Couldn't load this episode from GitHub.",
  // New: on a configured deployment "GitHub" would be the wrong (and, per
  // Q145-2, forbidden) noun for the source — this names the failure as the
  // deployment's own media policy instead.
  LOAD_ERROR_CONFIGURED:
    "Couldn't play this episode. This deployment only allows video from the source your admin configured, and this didn't come from it.",
  // Q145-1: the release page is GitHub's — only the default (unconfigured)
  // source ever links there. A configured deployment has no release page to
  // send anyone to, so this never renders alongside LOAD_ERROR_CONFIGURED.
  OPEN_RELEASE_PAGE: "Open the release page",
  ALL_EPISODES_TITLE: "All episodes",
  // Derived from EPISODES (recorded count, summed minutes) — never hand-typed,
  // so a re-shoot that ships/reserves an episode can't leave this stale.
  SUMMARY: (recorded: number, minutes: number) => `${recorded} recorded · about ${minutes} minutes · streamed from GitHub on click`,
  // New twin of SUMMARY for a configured source (Q145-2: no host/URL in text).
  SUMMARY_CONFIGURED: (recorded: number, minutes: number) =>
    `${recorded} recorded · about ${minutes} minutes · streamed from your admin's video source on click`,
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

