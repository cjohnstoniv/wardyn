/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Audit events + the run-detail supporting shapes projected from them.
export type ActorType = "human" | "agent" | "system";

export type Outcome = "success" | "failure" | "denied";

export interface AuditEvent {
  id: string;
  time: string;
  run_id?: string;
  actor_type: ActorType;
  actor: string;
  action: string;
  target?: string;
  outcome: Outcome;
  source_ip?: string;
  data?: Record<string, unknown>;
}

// --- Run detail supporting shapes (UI-side, projected from audit events) ---
export interface CredentialGrant {
  id: string;
  scope: string;
  audience: string;
  state: "active" | "expired" | "revoked";
  minted_at?: string;
  expires_at?: string;
  jti?: string;
}

export interface EgressDecision {
  id: string;
  time: string;
  domain: string;
  decision: "allow" | "deny" | "pending";
  bytes?: number;
}

// --- Why a run ended badly (M7(b)) -----------------------------------------
// The console half of the CLI's runFailureReason (cmd/wardyn/commands.go): the
// reason a run FAILED, was KILLED, or was auto-stopped is only ever in its
// audit trail, and the run page already fetches that trail. No new endpoint.
//
// The set is deliberately small and closed. A cause this build does not
// recognise is "unknown" — the block then renders the state and a link to the
// trail and says nothing more, rather than inventing advice (CONSOLE-RULES §10:
// never overclaim).
export type RunEndingKind =
  | "image" // run.build failed: the sandbox image could not be built, so nothing ran
  | "selftest" // run.selftest failed CLOSED: the wrapped image was refused before any task
  | "killed" // an operator killed it
  | "auto_stop" // the idle reaper stopped it — the policy working, not a fault
  | "unknown";

export interface RunEnding {
  kind: RunEndingKind;
  /** The audit action carrying the evidence ("run.kill", "run.build", …). */
  action: string;
  outcome?: Outcome;
  /** Who the audit row names — the killer, or wardynd for a system event. */
  actor?: string;
  /** When that event was recorded. */
  time?: string;
  /**
   * The failing step's own reason, verbatim off the audit row — the same
   * error/reason/detail keys, in the same order, the CLI's runFailureReason
   * reads (cmd/wardyn/commands.go). Absent when the row carried none.
   */
  detail?: string;
}
