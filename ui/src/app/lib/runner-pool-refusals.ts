/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The runner pool refusals the server and the console say in the same bytes: the
// wire reasons (internal/runnerpool/reasons.go) and their sentences. The console
// prints the server's sentence on a refusal and uses these for its own pre-check.
// Both sides are pinned to runner-pool-refusals.golden.json
// (internal/runnerpool, runner-pool-refusals.test.ts); change a sentence there,
// here and in that table together.

/** The closed set of pool selection reasons. */
export const RUNNER_POOL_REASONS = [
  "runner_pool_invalid",
  "runner_pool_required",
  "runner_pool_not_found",
  "runner_pool_unavailable",
  "runner_pool_default_unavailable",
  "runner_pool_stale",
  "runner_pool_no_eligible_member",
  "runner_pool_member_mismatch",
  "runner_pools_unavailable",
] as const;

export type RunnerPoolReason = (typeof RUNNER_POOL_REASONS)[number];

/** The console's name for a hosting type (runnerpool.HostingLabel). */
export const HOSTING_LABEL = { remote_provided: "Remote Provided", self_hosted: "Self-Hosted" } as const;

/** The sentences, byte for byte what internal/runnerpool/reasons.go says. */
export const RUNNER_POOL_REFUSAL = {
  INVALID: () => "runner_pool_id must be the id of a pool from your pool list.",
  REQUIRED: (label: string) => `Choose a ${label || "runner"} pool. No default pool applies to this run.`,
  REQUIRED_ANY: () => "Choose a runner pool. No default pool applies to this run.",
  NOT_FOUND: () => "That pool isn't available to you. Choose one from the list.",
  UNAVAILABLE: (name: string) => `${name} is switched off right now. Choose another pool.`,
  DEFAULT_UNAVAILABLE_PERSONAL: () => "Your default pool is no longer available. Choose a pool to continue.",
  DEFAULT_UNAVAILABLE_ORG: () => "Your organisation's default pool is no longer available. Choose a pool to continue.",
  STALE: (name: string) => `${name} changed while this run was being set up. Review it and try again.`,
  NO_ELIGIBLE_MEMBER: (name: string) => `No runner in ${name} can run this right now.`,
  NO_OWN_RUNNER: (name: string) => `None of your own runners in ${name} can run this right now.`,
  MEMBER_MISMATCH: (runner: string, name: string) => `${runner} is not one of your runners in ${name}.`,
  HOSTING_MISMATCH: (name: string, label: string) => `${name} is a ${label} pool, so it can't be used with this runner choice.`,
  UNAVAILABLE_SERVER: () => "This server does not manage runner pools yet.",
} as const;

/** The reason each sentence belongs to; the console matches on these, never on the text. */
export const RUNNER_POOL_REFUSAL_REASON: Record<keyof typeof RUNNER_POOL_REFUSAL, RunnerPoolReason> = {
  INVALID: "runner_pool_invalid",
  REQUIRED: "runner_pool_required",
  REQUIRED_ANY: "runner_pool_required",
  NOT_FOUND: "runner_pool_not_found",
  UNAVAILABLE: "runner_pool_unavailable",
  DEFAULT_UNAVAILABLE_PERSONAL: "runner_pool_default_unavailable",
  DEFAULT_UNAVAILABLE_ORG: "runner_pool_default_unavailable",
  STALE: "runner_pool_stale",
  NO_ELIGIBLE_MEMBER: "runner_pool_no_eligible_member",
  NO_OWN_RUNNER: "runner_pool_no_eligible_member",
  MEMBER_MISMATCH: "runner_pool_member_mismatch",
  HOSTING_MISMATCH: "runner_pool_member_mismatch",
  UNAVAILABLE_SERVER: "runner_pools_unavailable",
};

/** Every sentence function by key, for the parity test. */
export const RUNNER_POOL_REFUSAL_BY_KEY: Record<keyof typeof RUNNER_POOL_REFUSAL, (...args: string[]) => string> = {
  INVALID: () => RUNNER_POOL_REFUSAL.INVALID(),
  REQUIRED: (...a) => RUNNER_POOL_REFUSAL.REQUIRED(a[0]),
  REQUIRED_ANY: () => RUNNER_POOL_REFUSAL.REQUIRED_ANY(),
  NOT_FOUND: () => RUNNER_POOL_REFUSAL.NOT_FOUND(),
  UNAVAILABLE: (...a) => RUNNER_POOL_REFUSAL.UNAVAILABLE(a[0]),
  DEFAULT_UNAVAILABLE_PERSONAL: () => RUNNER_POOL_REFUSAL.DEFAULT_UNAVAILABLE_PERSONAL(),
  DEFAULT_UNAVAILABLE_ORG: () => RUNNER_POOL_REFUSAL.DEFAULT_UNAVAILABLE_ORG(),
  STALE: (...a) => RUNNER_POOL_REFUSAL.STALE(a[0]),
  NO_ELIGIBLE_MEMBER: (...a) => RUNNER_POOL_REFUSAL.NO_ELIGIBLE_MEMBER(a[0]),
  NO_OWN_RUNNER: (...a) => RUNNER_POOL_REFUSAL.NO_OWN_RUNNER(a[0]),
  MEMBER_MISMATCH: (...a) => RUNNER_POOL_REFUSAL.MEMBER_MISMATCH(a[0], a[1]),
  HOSTING_MISMATCH: (...a) => RUNNER_POOL_REFUSAL.HOSTING_MISMATCH(a[0], a[1]),
  UNAVAILABLE_SERVER: () => RUNNER_POOL_REFUSAL.UNAVAILABLE_SERVER(),
};
