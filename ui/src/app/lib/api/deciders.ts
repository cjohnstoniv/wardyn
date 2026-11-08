/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Unit deciders (internal/types/deciders.go): who besides the security tier may decide each kind of hold
// for runs launched in a unit. They arrive in GET /governance's `deciders` and are written by
// PUT /governance/units/{id}/deciders, which answers the stored rules or a held change (always held
// when a unit admin writes them).

import { asJson, wfetch } from "./core";

/** The kinds a rule may name (types.DeciderKinds). */
export type DeciderKind =
  | "egress_domain"
  | "ado_escalation"
  | "mcp_call"
  | "kube_call"
  | "credential"
  | "push_content"
  | "elevation"
  | "image"
  | "file_download";

/** types.DeciderRule: `who` is owner, unit_admins or group:<name>; empty means the security tier alone. */
export interface DeciderRule {
  kinds: DeciderKind[];
  who: string[];
  second_human?: boolean;
  /** once, until or run; never always. */
  max_scope?: "once" | "until" | "run";
  /** An elevation rule's longest grant, 15m to 24h, stored as one canonical duration ("4h" for "240m"). */
  max_elevation?: string;
  /** An elevation rule's who may ask: one to eight group:<name> entries; required on an elevation rule. */
  eligible_requesters?: string[];
  /** An elevation rule's wait after an elevation ends before the same person may ask again, 0s to 168h. */
  renew_after?: string;
  /** An elevation rule's ticket format, a whole-ticket regular expression of at most 256 characters. */
  ticket_pattern?: string;
}

/** types.UnitDeciders: the PUT's 200 body. */
export interface UnitDeciders {
  unit_id: string;
  rules: DeciderRule[];
  updated_by?: string;
  updated_at?: string;
}

/** governanceDecidersView: one unit's entry in GET /governance. `from` maps each kind to the nearest unit with a rule;
 *  `bounded_by` lists every unit on the chain with one, nearest first; `effective` is the rule they leave once met
 *  (an empty `who` means the security tier alone). */
export interface GovernanceDeciders {
  unit_id: string;
  rules: DeciderRule[];
  from?: Record<string, string>;
  bounded_by?: Record<string, string[]>;
  effective?: Record<string, DeciderRule>;
}

/** PUT /governance/units/{id}/deciders: replaces the unit's rules. A held write (always, for a unit admin)
 *  throws PendingChangeError; a refusal throws HttpError with its `reason` (invalid_deciders,
 *  decider_rule_wider_than_parent). */
export async function putUnitDeciders(unitId: string, rules: DeciderRule[]): Promise<UnitDeciders> {
  const res = await wfetch(`/governance/units/${encodeURIComponent(unitId)}/deciders`, {
    method: "PUT",
    asPerson: true,
    body: JSON.stringify({ rules }),
  });
  return asJson<UnitDeciders>(res);
}
