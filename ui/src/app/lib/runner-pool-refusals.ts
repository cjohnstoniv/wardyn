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
  "runner_pool_run_type_not_allowed",
  "runner_pool_barrier_not_allowed",
  "runner_pool_queue_full",
  "runner_pool_full",
] as const;

export type RunnerPoolReason = (typeof RUNNER_POOL_REASONS)[number];

/** What a listed pool's reason may hold (runnerpool.AvailabilityReasons), in the order the server checks them. */
export const POOL_AVAILABILITY_REASONS = [
  "runner_pool_unavailable",
  "placement_denied",
  "placement_unavailable",
  "runner_pool_no_eligible_member",
  "runner_pool_no_online_runner",
  "runner_pool_run_type_not_allowed",
  "runner_pool_not_found",
] as const;

export type PoolAvailabilityReason = (typeof POOL_AVAILABILITY_REASONS)[number];

/** Why a barrier is not available (runnerpool.BarrierReasons); when several apply, the first wins. */
export const BARRIER_REASON = ["below_floor", "policy_floor", "access_floor", "pool_not_allowed", "unknown", "not_offered"] as const;

export type BarrierReason = (typeof BARRIER_REASON)[number];

/**
 * A user pool whose own runners are all offline (runnerpool.NoOnlineRunnerMsg): the pool's
 * runner_pool_no_online_runner and a launch's runner_offline say this sentence.
 */
export const POOL_OFFLINE = {
  NO_ONLINE_RUNNER: (name: string) => `None of your runners in ${name} is online.`,
} as const;

/**
 * A run that would start at once while every runner in the pool is at one of its limits, its Runs at
 * once or a total (runnerpool.RunnerAtCapacityMsg): the create and preflight runner_at_capacity. A pool
 * none of whose runners has its limits set (runnerpool.RunnerLimitsUnsetMsg): runner_limits_unset.
 */
export const RUNNER_CAPACITY = {
  RUNNER_AT_CAPACITY: (name: string) =>
    `Every runner in ${name} is at its limit. Try again when a run there finishes, or raise the runner's limits.`,
  RUNNER_LIMITS_UNSET: (name: string) => `No runner in ${name} has its limits set. Every runner needs limits before it can run anything.`,
} as const;

/** How a sentence names runs of a type (types.RunnerPoolRunType.Plural). */
export const RUN_TYPE_PLURAL = { background: "background tasks", interactive: "interactive environments" } as const;

/** The console's name for a barrier (types.BarrierLabel). */
export const BARRIER_LABEL = { CC1: "Fence", CC2: "Wall", CC3: "Vault" } as const;

/** Why the platform ended a run (types.RunEndReason), with the sentence the run detail and the audit view say. */
export const RUN_END_REASON_SENTENCE = {
  max_lifetime_reached: "This run reached its maximum lifetime and was ended.",
  end_reached: "This run reached its end time and was ended.",
  queue_timeout: "This run waited too long for a free slot and was not started.",
  queue_profile_changed: "Your governance profile changed while this run waited. Start it again.",
  queue_policy_changed: "The policy this run used changed while it waited. Start it again to review it.",
  queue_portal_revoked: "The portal that launched this run was removed while it waited.",
  runner_pool_removed: "The pool this run was waiting for was removed.",
  identity_deactivated: "This account's access has been removed. Sign in again, or ask your Wardyn admin.",
} as const;

/** The sentences, byte for byte what internal/runnerpool/reasons.go says. */
export const RUNNER_POOL_REFUSAL = {
  INVALID: () => "runner_pool_id must be the id of a pool from your pool list.",
  REQUIRED: () => "Choose a runner pool. No default pool applies to this run.",
  NOT_FOUND: () => "That pool isn't available to you. Choose one from the list.",
  UNAVAILABLE: (name: string) => `${name} is switched off right now. Choose another pool.`,
  DEFAULT_UNAVAILABLE_PERSONAL: () => "Your default pool is no longer available. Choose a pool to continue.",
  DEFAULT_UNAVAILABLE_ORG: () => "Your organisation's default pool is no longer available. Choose a pool to continue.",
  STALE: (name: string) => `${name} changed while this run was being set up. Review it and try again.`,
  NO_ELIGIBLE_MEMBER: (name: string) => `No runner in ${name} can run this right now.`,
  NO_OWN_RUNNER: (name: string) => `None of your own runners in ${name} can run this right now.`,
  MEMBER_MISMATCH: (runner: string, name: string) => `${runner} is not one of your runners in ${name}.`,
  KIND_MISMATCH: (name: string) => `${name} has no runner of the kind this run asks for. Choose another pool.`,
  ORG_POOL_SELF_HOSTED: () => "Organization pools take remote-hosted runners only. A self-hosted runner can join its owner's user pools.",
  REMOTE_NOT_CONFIGURED: (id: string) => `${id} isn't a remote-hosted runner this server is set up with.`,
  REMOTE_NOT_AVAILABLE: (id: string) => `${id} is not available to people's pools. Ask your admin to make it available.`,
  UNAVAILABLE_SERVER: () => "This server does not manage runner pools yet.",
  RUN_TYPE_NOT_ALLOWED: (name: string, runType: keyof typeof RUN_TYPE_PLURAL) =>
    `${name} doesn't take ${RUN_TYPE_PLURAL[runType]}. Choose another pool or run type.`,
  BARRIER_NOT_ALLOWED: (name: string, asked: keyof typeof BARRIER_LABEL, allowed: (keyof typeof BARRIER_LABEL)[]) => {
    const labels = allowed.map((c) => BARRIER_LABEL[c]);
    const list = labels.length > 1 ? `${labels.slice(0, -1).join(", ")} and ${labels[labels.length - 1]}` : labels.join("");
    return `${name} doesn't allow the ${BARRIER_LABEL[asked]} barrier. It allows ${list}.`;
  },
  QUEUE_FULL: (name: string, n: number) => `You already have ${n} runs waiting for ${name}. Cancel one or wait for one to start.`,
  FULL: (name: string, n: number) => `${name} is running its limit of ${n}, and others are waiting. Try again when one finishes.`,
} as const;

/**
 * Why a run waits instead of starting (runnerpool.Wait). Not a refusal: no HTTP status, never in
 * RUNNER_POOL_REASONS, and the console shows it on a queued run, not on a rejected launch.
 */
export const RUN_WAIT = {
  AT_CAPACITY: (name: string, max: number) => `${name} is running its limit of ${max}. This run starts when there's room.`,
  NO_RUNNER_FREE: (name: string) => `None of your runners in ${name} is online. This run starts when one is.`,
  RUNNERS_AT_CAPACITY: (name: string) => `Every runner in ${name} is at its limit. This run starts when one has room.`,
} as const;

/** The reason a waiting run carries for each RUN_WAIT sentence. */
export const RUN_WAIT_REASON: Record<keyof typeof RUN_WAIT, string> = {
  AT_CAPACITY: "runner_pool_at_capacity",
  NO_RUNNER_FREE: "runner_pool_no_runner_free",
  RUNNERS_AT_CAPACITY: "runner_pool_runners_at_capacity",
};

/** The reason each sentence belongs to; the console matches on these, never on the text. */
export const RUNNER_POOL_REFUSAL_REASON: Record<keyof typeof RUNNER_POOL_REFUSAL, RunnerPoolReason> = {
  INVALID: "runner_pool_invalid",
  REQUIRED: "runner_pool_required",
  NOT_FOUND: "runner_pool_not_found",
  UNAVAILABLE: "runner_pool_unavailable",
  DEFAULT_UNAVAILABLE_PERSONAL: "runner_pool_default_unavailable",
  DEFAULT_UNAVAILABLE_ORG: "runner_pool_default_unavailable",
  STALE: "runner_pool_stale",
  NO_ELIGIBLE_MEMBER: "runner_pool_no_eligible_member",
  NO_OWN_RUNNER: "runner_pool_no_eligible_member",
  MEMBER_MISMATCH: "runner_pool_member_mismatch",
  KIND_MISMATCH: "runner_pool_member_mismatch",
  ORG_POOL_SELF_HOSTED: "runner_pool_member_mismatch",
  REMOTE_NOT_CONFIGURED: "runner_pool_member_mismatch",
  REMOTE_NOT_AVAILABLE: "runner_pool_member_mismatch",
  UNAVAILABLE_SERVER: "runner_pools_unavailable",
  RUN_TYPE_NOT_ALLOWED: "runner_pool_run_type_not_allowed",
  BARRIER_NOT_ALLOWED: "runner_pool_barrier_not_allowed",
  QUEUE_FULL: "runner_pool_queue_full",
  FULL: "runner_pool_full",
};

/** Every sentence function by key, for the parity test. */
export const RUNNER_POOL_REFUSAL_BY_KEY: Record<keyof typeof RUNNER_POOL_REFUSAL, (...args: string[]) => string> = {
  INVALID: () => RUNNER_POOL_REFUSAL.INVALID(),
  REQUIRED: () => RUNNER_POOL_REFUSAL.REQUIRED(),
  NOT_FOUND: () => RUNNER_POOL_REFUSAL.NOT_FOUND(),
  UNAVAILABLE: (...a) => RUNNER_POOL_REFUSAL.UNAVAILABLE(a[0]),
  DEFAULT_UNAVAILABLE_PERSONAL: () => RUNNER_POOL_REFUSAL.DEFAULT_UNAVAILABLE_PERSONAL(),
  DEFAULT_UNAVAILABLE_ORG: () => RUNNER_POOL_REFUSAL.DEFAULT_UNAVAILABLE_ORG(),
  STALE: (...a) => RUNNER_POOL_REFUSAL.STALE(a[0]),
  NO_ELIGIBLE_MEMBER: (...a) => RUNNER_POOL_REFUSAL.NO_ELIGIBLE_MEMBER(a[0]),
  NO_OWN_RUNNER: (...a) => RUNNER_POOL_REFUSAL.NO_OWN_RUNNER(a[0]),
  MEMBER_MISMATCH: (...a) => RUNNER_POOL_REFUSAL.MEMBER_MISMATCH(a[0], a[1]),
  KIND_MISMATCH: (...a) => RUNNER_POOL_REFUSAL.KIND_MISMATCH(a[0]),
  ORG_POOL_SELF_HOSTED: () => RUNNER_POOL_REFUSAL.ORG_POOL_SELF_HOSTED(),
  REMOTE_NOT_CONFIGURED: (...a) => RUNNER_POOL_REFUSAL.REMOTE_NOT_CONFIGURED(a[0]),
  REMOTE_NOT_AVAILABLE: (...a) => RUNNER_POOL_REFUSAL.REMOTE_NOT_AVAILABLE(a[0]),
  UNAVAILABLE_SERVER: () => RUNNER_POOL_REFUSAL.UNAVAILABLE_SERVER(),
  RUN_TYPE_NOT_ALLOWED: (...a) => RUNNER_POOL_REFUSAL.RUN_TYPE_NOT_ALLOWED(a[0], a[1] as keyof typeof RUN_TYPE_PLURAL),
  BARRIER_NOT_ALLOWED: (...a) =>
    RUNNER_POOL_REFUSAL.BARRIER_NOT_ALLOWED(a[0], a[1] as keyof typeof BARRIER_LABEL, a.slice(2) as (keyof typeof BARRIER_LABEL)[]),
  QUEUE_FULL: (...a) => RUNNER_POOL_REFUSAL.QUEUE_FULL(a[0], Number(a[1])),
  FULL: (...a) => RUNNER_POOL_REFUSAL.FULL(a[0], Number(a[1])),
};
