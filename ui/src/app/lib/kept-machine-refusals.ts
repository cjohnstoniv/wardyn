/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The wire reasons of the kept-machine refusals (internal/api/reasons_kept_machine.go). The console prints the
// server's own sentence for each, so only the reasons are named here: New Run sends a refusal of a run's machine to the
// Run step, where the machine is chosen, and marks the Where row.

export const KEPT_MACHINE_REASON = {
  SITE_OFF: "kept_machine_site_off",
  NOT_YOUR_MAC: "kept_machine_not_your_mac",
  PERSON_LIMIT: "kept_machine_person_limit",
  NOT_FOUND: "kept_machine_not_found",
  IN_USE: "kept_machine_in_use",
  DELETING: "kept_machine_deleting",
  DELETE_FAILED: "kept_machine_delete_failed",
  DELETE_UNCONFIRMED: "kept_machine_delete_unconfirmed",
  INVALID: "kept_machine_invalid",
  INTERACTIVE_ONLY: "kept_machine_interactive_only",
  RESIDENT_SECRETS: "kept_machine_resident_secrets",
  WITH_WORK_FOLDER: "kept_machine_with_work_folder",
  EXPIRED: "kept_machine_expired",
  BASE_TOO_OLD: "kept_machine_base_too_old",
  OFFLINE: "kept_machine_offline",
  RUNNER_TOO_OLD: "kept_machine_runner_too_old",
  MAC_LIMIT: "kept_machine_mac_limit",
  NAME_TAKEN: "kept_machine_name_taken",
  UNAVAILABLE: "kept_machine_unavailable",
  FRESH_BASE: "kept_machine_fresh_base",
  RESET_UNCONFIRMED: "kept_machine_reset_unconfirmed",
} as const;

export type KeptMachineReason = (typeof KEPT_MACHINE_REASON)[keyof typeof KEPT_MACHINE_REASON];
