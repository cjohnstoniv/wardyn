/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Restrictions (internal/types/restriction.go): deny-only lists that follow people, in every unit they run in.
// They arrive in GET /governance's `restrictions` and, for the caller, in GET /policies/default. The write routes
// are POST/PUT/DELETE /governance/restrictions (securityOps; a delete is always held under the second-person switch).
import type { ConfinementClass } from "../types/runs";
import type { FirstUseMode, GrantSpec } from "../types/policy";
import type { PolicyContact } from "../types/site";
import type { PlacementValue } from "../types/new-run-contract";

/** types.RestrictionSpec: a knob it does not name is no limit, and a bool can only be false. */
export interface RestrictionSpec {
  denied_domains?: string[];
  allow_all_egress?: boolean;
  first_use_approval?: FirstUseMode;
  first_use_hold_seconds?: number;
  max_holds?: number;
  min_confinement_class?: ConfinementClass;
  auto_stop_after_sec?: number;
  git_push_any_branch?: boolean;
  /** Credentials the run is not given: a scope-less entry names every grant of its kind. */
  withheld_grants?: GrantSpec[];
}

/** types.RestrictionLimits: the deny_* switches only turn on; the other bools can only be false. */
export interface RestrictionLimits {
  local_self_defined_components?: boolean;
  deny_task_mode_exec?: boolean;
  deny_interactive?: boolean;
  deny_ui_apps?: boolean;
  deny_user_drive?: boolean;
  deny_member_drives?: boolean;
  deny_placements?: PlacementValue[];
  max_concurrent_runs?: number;
  max_cpu_millis?: number;
  max_memory_mib?: number;
  max_ephemeral_disk_mib?: number;
  max_drive_size_mib?: number;
  max_end_ahead_sec?: number;
  max_wait_sec?: number;
  pause_idle_after_sec?: number;
  allow_no_end?: boolean;
  user_changes_limits?: boolean;
}

/** types.RestrictionMember: a user, a group, a user type, or everyone. */
export interface RestrictionMember {
  subject_type: string;
  subject: string;
}

/** types.RestrictionWrite: the POST/PUT body (a PUT replaces the members too). */
export interface RestrictionWrite {
  name: string;
  spec: RestrictionSpec;
  limits: RestrictionLimits;
  contact?: PolicyContact;
  members: RestrictionMember[];
}

/** types.Restriction. */
export interface Restriction {
  id: string;
  name: string;
  spec: RestrictionSpec;
  limits: RestrictionLimits;
  contact?: PolicyContact;
  members: RestrictionMember[];
  created_by?: string;
  created_at: string;
  updated_at: string;
}

/** restrictionView (GET /governance): a restriction with how many people it names. */
export interface RestrictionView extends Restriction {
  people: number;
}
