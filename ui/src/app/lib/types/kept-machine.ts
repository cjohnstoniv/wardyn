/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Kept macOS VM machines on the wire: the list a person reads (pkg/client/machines.go) and the machine a run reports
// (internal/api/run_kept_machine.go's runMachineView). wire-parity.test.ts pins the keys.

/** Go: types.KeptMachineState. */
export type KeptMachineState = "creating" | "in_use" | "kept" | "deleting";

/** Go: client.KeptMachineDeletedBy*; set only while the machine is being deleted. */
export type KeptMachineDeleteReason = "person" | "expired" | "admin" | "erasure";

/** Go: client.KeptMachine. */
export interface KeptMachine {
  id: string;
  name: string;
  owner_subject: string;
  runner_id: string;
  base: string;
  base_prepared_at: string;
  state: KeptMachineState;
  run_id?: string;
  /** False after a stop that was not graceful: Wardyn removes the last run's settings when the machine next starts. */
  scrubbed: boolean;
  disk_bytes: number;
  created_at: string;
  last_used_at: string;
  expires_at: string;
  /** Set once a delete has started and is still finishing. */
  delete_started_at?: string;
  delete_reason?: KeptMachineDeleteReason;
  /** The Mac the machine is kept on, filled by the server. */
  runner_name?: string;
}

/**
 * Go: client.MyMachines. `reason` is the refusal code when `allowed` is false: kept_machine_site_off, governance_unit,
 * kept_machine_not_your_mac, or kept_machine_person_limit (resuming one still works).
 */
export interface MyMachines {
  machines: KeptMachine[];
  allowed: boolean;
  reason?: string;
  count: number;
  limit: number;
  /** Days a machine may sit unused before Wardyn deletes it: the site's setting. Absent from a server that does not say. */
  idle_days?: number;
  /** The most disk one new kept machine can take, in bytes. */
  worst_bytes: number;
}

/** Go: client.RunMachine, the create request's machine. Keep and resume are exclusive. */
export interface RunMachineRequest {
  keep?: boolean;
  name?: string;
  /** The kept machine this run starts from, by id or name. */
  resume?: string;
  /** With resume: reset the machine to its base before the run starts. */
  fresh?: boolean;
}

/** Go: internal/api.runMachineView, GET /runs/{id}'s machine. */
export interface RunMachine {
  id: string;
  name?: string;
  mode?: "new" | "resumed" | "fresh";
  keep_at_end?: boolean;
  state?: KeptMachineState;
  scrubbed: boolean;
  runner_name?: string;
  /** The run that holds the machine now, or held it last (a stop does not clear it). The row is shared by every run that used the machine. */
  holder_run_id?: string;
  base?: string;
  /** What the last stop wrote, in bytes. */
  disk_bytes?: number;
  expires_at?: string;
  /** The machine's row is gone: who deleted it, and why, stays in the audit log. */
  deleted: boolean;
}
