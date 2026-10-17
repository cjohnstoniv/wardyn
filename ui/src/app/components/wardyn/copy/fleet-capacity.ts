/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M6 (approved 2026-10-03) — the admin Fleet capacity card's canon strings.
// A figure that differs per run (a count, a size, a time) is a function
// argument, never re-worded. Every figure is a configured reservation, never a
// measurement: the card must not say "used" or "allocated".

export const FLEET_CAPACITY = {
  TITLE: "Fleet capacity",
  SUMMARY: (n: number, cpu: string, mem: string) => `${n} runs · ${cpu} CPU · ${mem} GiB configured reservations`,
  SUMMARY_UNKNOWN: (u: number) => `· ${u} unknown`,
  SUMMARY_NONE: "No runs are holding capacity",
  LEDE: "What Wardyn asked the runner to set aside for runs still going. Configured, not measured.",
  STATES_HINT: "Pending runs hold nothing yet. Kept runs have lost their sandbox and hold nothing.",
  RUNNER_K8S: "Kubernetes · requests",
  RUNNER_DOCKER: "Docker · caps",
  BASIS_REQUESTS: "What the scheduler sets aside and your namespace quota charges. Limits are shown beside them.",
  BASIS_CAPS: "Docker sets nothing aside. These are the most the sandboxes may take.",
  AGENT_REQUESTS: "Agent requests",
  AGENT_LIMITS: "Agent limits",
  AGENT_CAPS: "Agent caps",
  PROXY: "Proxy",
  NO_CAP: "no cap",
  UNKNOWN: (u: number) => `${u} unknown — not in these totals.`,
  UNKNOWN_HINT:
    "Runs started before 0.8.6 recorded no reservation, and a run can be unknown for a moment while it starts. They leave this count when they end.",
  WAITING_CHIP: (n: number) => `Waiting for room · ${n}`,
  WAITING_LEAD: "They still count against your quota.",
  WAITING_SINCE: (rel: string) => `since ${rel}`,
  WAITING_MORE: (shown: number, total: number) => `Showing the oldest ${shown} of ${total}.`,
  AGE_TITLE: "By age",
  OWNERS_TITLE: "Top owners",
  OWNERS_COLS: { OWNER: "Owner", RUNS: "Runs", CPU: "CPU", MEMORY: "Memory" },
  OWNERS_SHOW_ALL: (n: number) => `Show all ${n}`,
  OWNERS_TRUNCATED: "Only the top 50 owners by CPU are listed.",
  // Paused and Kept are not run states, so their state-line labels live here;
  // the other four are RunStateBadge's labels (runStateLabel).
  STATES_PAUSED: "Paused",
  STATES_KEPT: "Kept",
  RESIDUAL_NOTE: "A sandbox that outlived its ended run is not counted here.",
} as const;

export const FLEET_CAPACITY_AGE_LABELS: Record<string, string> = {
  under_1h: "Under 1h",
  "1h_to_8h": "1–8h",
  "8h_to_24h": "8–24h",
  "1d_to_7d": "1–7d",
  over_7d: "Over 7d",
};
