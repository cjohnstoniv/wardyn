/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What a workload's last refusal says (the drawer's "Last refusal", mock r16 T3-a, owner answer q12): a plain
// sentence first and the reason code in small print. The server stores the reason a launch was refused with, and
// the sentence the create path gave. The reasons the workload paths send (internal/api/reasons.go) and the ones a
// launch through the create path stores for a pool-bound workload are told here; any other reason reads as the
// server's own sentence, which is already plain. workload-refusals.test.ts pins each key to a Go constant.

export const WORKLOAD_REFUSAL_SENTENCE: Record<string, string> = {
  // The workload paths (internal/api/reasons.go).
  workload_binding_inactive: "The namespace binding no longer carries authority, so nothing launches until it is bound again.",
  workload_binding_pool_scope: "A binding names an organization pool, not a person's own.",
  workload_pool_mismatch: "The spec names a runner pool other than the one its namespace is bound on.",
  workload_invalid: "The spec is not a workload Wardyn accepts.",
  workload_source_conflict: "Another writer already owns a workload of this name.",
  workload_cap_reached: "The owner already holds the most workloads allowed.",
  service_identity_inactive: "The service identity is suspended or retired, or its sponsor's session is not live.",
  internal_error: "Wardyn failed while launching it.",
  // The pool a launch goes to (internal/runnerpool/reasons.go). A custom resource's runs go to its binding's pool only.
  runner_pool_invalid: "The runner pool the spec names is not a valid pool id.",
  runner_pool_required: "No runner pool was chosen and no default applies.",
  runner_pool_not_found: "The runner pool it is bound to is gone.",
  runner_pool_unavailable: "The runner pool is switched off.",
  runner_pool_default_unavailable: "The default runner pool is no longer available.",
  runner_pool_stale: "The runner pool changed while the run was being set up.",
  runner_pool_no_eligible_member: "No runner in the pool can run it right now.",
  runner_pool_member_mismatch: "The runner it names is not in the pool.",
  runner_pools_unavailable: "This server does not manage runner pools.",
  runner_pool_run_type_not_allowed: "The pool does not take this type of run.",
  runner_pool_run_type_missing: "The spec does not say whether the run is background or interactive.",
  runner_pool_barrier_not_allowed: "The pool does not allow the barrier the spec asks for.",
  runner_pool_queue_full: "The pool's queue is full.",
  runner_pool_full: "The pool is running its limit of runs.",
  // The runner it would land on (internal/placement/reasons.go).
  runner_offline: "No runner that could take it is online.",
  runner_at_capacity: "Every runner in the pool is at its limit.",
  runner_limits_unset: "No runner in the pool has its limits set.",
  runner_recording_off: "Recording is required, and no runner in the pool records runs.",
  recording_store_off: "Recording is required, and this organisation keeps no session recordings.",
  placement_credential: "A credential in its policy, often a grant from the default policy, cannot be sent to the runner it would land on.",
};

// The reasons whose server sentence follows the table's: it names the credential and how to drop it. A reader the
// server withholds the sentence from (the security tier) sees the table's alone.
const WITH_SERVER_SENTENCE = new Set(["placement_credential"]);

/** The drawer's sentence for a refusal: the table's when the reason is told, else the server's own, else a plain fallback. */
export function refusalSentence(reason: string, message?: string): string {
  const told = WORKLOAD_REFUSAL_SENTENCE[reason];
  const own = message?.trim();
  if (told && own && WITH_SERVER_SENTENCE.has(reason)) return `${told} ${own}`;
  return told ?? (own || "The launch was refused.");
}
