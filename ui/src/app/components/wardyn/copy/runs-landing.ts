/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 D2 — the Runs landing page's own new/changed canon strings
// (docs handoff design.md §2). Reused strings (STATES.*, RUNS_MEMBER_EMPTY.*,
// RUN_WAIT.*, waitingReauth/waitingAdoConsent, ownerLabel) stay in their own
// modules — this file holds only what is NEW or CHANGED for this page.

export const RUNS_SECTION = {
  NEEDS: "Needs you",
  NEEDS_ADMIN: "Needs a decision",
  // H-3, Admin view only: owners' sign-ins and lost runs — no admin can act.
  OWNER: "Waiting on the owner",
  RUNNING: "Running",
  ENDED_TODAY: "Ended today",
  EARLIER: "Earlier this week",
  EARLIER_MONTH: "Earlier this month",
  OLDER: "Older",
} as const;

export const RUNS_QUIET = "Nothing needs you and nothing is running.";

// The ageing note (design.md §2.1 AGED). `older` and `killed` are counts of
// runs hidden by the ended-within window and the killed-24h rule respectively
// — mutually exclusive per run, so the two counts never double-count one row.
export function runsAgedNote(older: number, killed: number, windowLabel: string): string {
  const parts: string[] = [];
  if (older > 0) parts.push(`${older} ${older === 1 ? "older run" : "older runs"}`);
  if (killed > 0) parts.push(`${killed} ${killed === 1 ? "killed run" : "killed runs"}`);
  const verb = older + killed === 1 ? "is" : "are";
  return `Showing the ${windowLabel}. ${parts.join(" and ")} ${verb} hidden.`;
}

export const RUNS_AGED_WINDOW_LABEL: Record<string, string> = {
  "24h": "last 24 hours",
  "7d": "last 7 days",
  "30d": "last 30 days",
  all: "full history",
};

export const RUNS_SHOW_30 = "Show the last 30 days";
export const RUNS_INCLUDE_KILLED = "Include killed";

// #215-style rename over the pre-#1197 facet copy: "facet" -> "filter", and
// the sentence is now shared by both the search box and every select above it
// (there is one filter bar, not a search box plus separate facets).
export const RUNS_NO_MATCH_BODY = "Try a different search term or filter.";

export const RUNS_COMPOSER = {
  LABEL: "Start a run",
  PLACEHOLDER: "Describe the task in your own words. It becomes the run's title.",
  WORKSPACE: "Workspace",
  GO: "Start run",
} as const;

export const RUNS_FILTERS = {
  SEARCH_PLACEHOLDER: "Search titles, workspaces, people",
  STATUS_LABEL: "Status",
  STATUS_ALL: "Status · All",
  STATUS_NEEDS: "Needs you",
  STATUS_ACTIVE: "Active",
  STATUS_ENDED: "Ended",
  STATUS_FAILED: "Failed",
  STATUS_KILLED: "Killed",
  ENDED_WITHIN_LABEL: "Ended within",
  ENDED_24H: "Last 24 hours",
  ENDED_7D: "Last 7 days",
  ENDED_30D: "Last 30 days",
  ENDED_ALL: "All time",
  WORKSPACE_LABEL: "Workspace",
  WORKSPACE_ALL: "Workspace · All",
  // H-4, Admin view only.
  WHOSE_RUNS_LABEL: "Whose runs",
  EVERYONE: "Everyone",
  MINE: "Mine",
  // H-6.
  GROUP_LABEL: "Group by",
  GROUP_SECTIONS: "Group · Sections",
  GROUP_WORKSPACE: "Group · Workspace",
  GROUP_TITLE: "Group · Title",
  // H-8.
  SAVED_VIEW_LABEL: "Saved view",
} as const;

// H-8: saved views (localStorage, up to 10). "Save view" names the current
// URL. SAVED is a function (name interpolates) so it stays one string, not
// a template the caller reassembles.
export const RUNS_SAVE_VIEW = "Save view";
export const RUNS_SAVE_VIEW_NAME_LABEL = "Name this view";
export function runsViewSaved(name: string): string {
  return `Saved “${name}” in this browser. The link in the address bar shares it.`;
}

// The Saved view select's own option text, and the one shown when the
// current URL matches no saved view — the mock never leaves that select
// blank (home-runs-1197-packet.html:615).
export function runsSavedViewOption(name: string): string {
  return `View · ${name}`;
}
export const RUNS_SAVED_VIEW_CUSTOM = "View · Custom";

// The save-view form's own literals (home-runs-1197-packet.html:617): a
// prefilled name, and "Save" — distinct from the "Save view" button that
// opens the form.
export const RUNS_SAVE_VIEW_DEFAULT_NAME = "My view";
export const RUNS_SAVE_VIEW_SUBMIT = "Save";
export const RUNS_SAVE_VIEW_CANCEL = "Cancel";

// Row-state words that are NEW for this page (design.md §2.2). Reused words
// (Completed/Failed/Killed/Stopped/Archived) are literals directly in
// runs-model.ts's rowPresentation — this table only carries what D2 adds or
// changes.
export const RUNS_ROW_WORD = {
  NEEDS_APPROVAL: "Needs your approval",
  NEEDS_DECISION: "Needs a decision",
  WAITING_FOR_ADMIN: "Waiting for an admin",
  SANDBOX_STOPPED: "Sandbox stopped",
  RUNNING: "Running",
  STARTING: "Starting",
  QUEUED: "Queued",
} as const;

export const RUNS_ROW_ACTION = {
  REVIEW: "Review",
  SIGN_IN: "Sign in",
} as const;
