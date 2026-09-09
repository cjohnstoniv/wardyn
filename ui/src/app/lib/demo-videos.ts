/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Pure data: the demo video catalog. No React, no fetch — this is what a
// screen renders from, not a screen itself. `steps` is `string[]`, not a
// `SetupStepId[]`: this file must not import a screen module to stay pure.
//
// Every SHIPPED entry's `tag` is the literal string "v0.6.0", not a shared
// constant. cmd/wardynd/demo_videos_guard_test.go greps this file's own
// source text for `tag:`/`file:` literals and cross-checks them against
// README.md's release links — a constant would be invisible to that regex.
// A re-shoot's "one sed" (RELEASING.md step 7) rewrites the literals in both
// files at once, which is also why they have to stay literals here.
export interface Episode {
  id: string;
  title: string;
  audience: "admin" | "member" | "everyone";
  /** Shape C (approved mock round 2026-08-31): which deployment path the
   *  episode belongs to. `core` = watch first on any install; `single` /
   *  `multi` = that deployment's own setup story; `any` = running work,
   *  identical on both. Presentation grouping only — ids, files and the
   *  recorder's --video dispatch never key on it. */
  path: "core" | "single" | "multi" | "any";
  tag: string | null;
  file: string;
  minutes?: string;
  steps: string[];
}

// 11 shipped episodes (titles/lengths/filenames verbatim from README.md's own
// table) plus 13 reserved ids the series already has a shape for
// (scripts/lib/verify-demo-take-optionals.sh, docs/DEMO-SCRIPT.md) but that
// have not been recorded yet — `tag: null` until a release ships them, at
// which point `episodeUrl` starts resolving them.
//
// 00 IS FIRST, and the position is the claim: it is the front door, the one
// episode written for someone who knows only that they want to sandbox an
// agent, and 01 is what it hands off to.
export const EPISODES: Episode[] = [
  { id: "00", title: "Meet Wardyn", audience: "everyone", path: "core", tag: "v0.7.0", file: "wardyn-00-meet-wardyn.mp4", minutes: "5:53", steps: [] },
  { id: "01", title: "Why govern agents", audience: "everyone", path: "core", tag: "v0.7.0", file: "wardyn-01-why-govern-agents.mp4", minutes: "6:50", steps: [] },
  { id: "02", title: "Set up the host", audience: "admin", path: "single", tag: "v0.7.0", file: "wardyn-02-set-up-the-host.mp4", minutes: "7:26", steps: ["environment"] },
  { id: "02b", title: "Managed desktop", audience: "admin", path: "single", tag: null, file: "wardyn-02b-managed-desktop.mp4", steps: ["environment"] },
  { id: "02c", title: "One command to a cluster", audience: "admin", path: "multi", tag: null, file: "wardyn-02c-one-command-to-a-cluster.mp4", steps: ["environment"] },
  { id: "03a", title: "What it stops", audience: "everyone", path: "core", tag: "v0.7.0", file: "wardyn-03a-what-it-stops.mp4", minutes: "11:45", steps: ["corp_network"] },
  { id: "03b", title: "The network, three more ways", audience: "everyone", path: "core", tag: "v0.7.0", file: "wardyn-03b-the-network-three-more-ways.mp4", minutes: "5:05", steps: ["corp_network"] },
  { id: "03c", title: "Authorized, then issued", audience: "everyone", path: "core", tag: null, file: "wardyn-03c-authorized-then-issued.mp4", minutes: "7:26", steps: ["integrations"] },
  { id: "03d", title: "The kinds that can't use a header", audience: "everyone", path: "core", tag: "v0.7.0", file: "wardyn-03d-the-kinds-that-cant-use-a-header.mp4", minutes: "5:58", steps: ["integrations"] },
  { id: "04", title: "Add a workspace", audience: "admin", path: "single", tag: "v0.7.0", file: "wardyn-04-add-a-workspace.mp4", minutes: "4:39", steps: ["workspaces"] },
  { id: "04b", title: "A member's own workspace", audience: "member", path: "multi", tag: null, file: "wardyn-04b-a-members-own-workspace.mp4", steps: ["workspace"] },
  { id: "04c", title: "Who may do what", audience: "admin", path: "multi", tag: null, file: "wardyn-04c-who-may-do-what.mp4", steps: ["people"] },
  { id: "04d", title: "Your drive", audience: "member", path: "multi", tag: null, file: "wardyn-04d-your-drive.mp4", steps: ["workspace"] },
  { id: "05", title: "Your first policy", audience: "everyone", path: "core", tag: "v0.7.0", file: "wardyn-05-your-first-policy.mp4", minutes: "3:58", steps: [] },
  { id: "06", title: "Your first run", audience: "member", path: "any", tag: "v0.7.0", file: "wardyn-06-your-first-run.mp4", minutes: "3:58", steps: ["first-run"] },
  { id: "07", title: "Interactive runs", audience: "member", path: "any", tag: null, file: "wardyn-07-interactive-runs.mp4", minutes: "4:04", steps: ["first-run"] },
  { id: "08", title: "An autonomous agent", audience: "member", path: "any", tag: null, file: "wardyn-08-autonomous-agent.mp4", minutes: "4:14", steps: ["first-run"] },
  { id: "09", title: "Record a run", audience: "everyone", path: "core", tag: "v0.7.0", file: "wardyn-09-record-a-run.mp4", minutes: "6:06", steps: [] },
  { id: "10", title: "Approvals and egress", audience: "member", path: "any", tag: "v0.7.0", file: "wardyn-10-approvals-and-egress.mp4", minutes: "3:56", steps: ["approvals"] },
  { id: "11", title: "CI and headless", audience: "everyone", path: "any", tag: null, file: "wardyn-11-ci-and-headless.mp4", steps: [] },
  { id: "12", title: "Audit and attach", audience: "admin", path: "multi", tag: null, file: "wardyn-12-audit-and-attach.mp4", steps: [] },
  { id: "12b", title: "Admin operations", audience: "admin", path: "multi", tag: null, file: "wardyn-12b-admin-operations.mp4", steps: [] },
  { id: "13", title: "Your terminal, our cluster", audience: "member", path: "multi", tag: null, file: "wardyn-13-your-terminal-our-cluster.mp4", steps: [] },
];

// The member Getting Started screen (screens/onboarding/member-getting-started.tsx)
// imports this list — never the reverse, lib/ must not import a screen — and
// demo-videos.test.ts treats it as the allowed set of member step ids.
// COUPLING: rename a member section and update this list AND every member
// episode's `steps` entry together — otherwise `episodesFor` silently returns
// nothing for it.
export const MEMBER_SECTION_IDS = ["workspace", "first-run", "approvals"] as const;

// null tag = not recorded yet — never build a URL for a release that doesn't
// exist.
export function episodeUrl(e: Episode): string | null {
  if (e.tag === null) return null;
  return `https://github.com/cjohnstoniv/wardyn/releases/download/${e.tag}/${e.file}`;
}

export function releasePageUrl(tag: string): string {
  return `https://github.com/cjohnstoniv/wardyn/releases/tag/${tag}`;
}

export function episodesFor(stepId: string): Episode[] {
  return EPISODES.filter((e) => e.steps.includes(stepId));
}
