/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The policy editor's Azure DevOps capability lock across the person's organisations (#2146). Each of the
// person's rows (GET /me/scm-access) carries its own capability_ceiling. A capability is locked only when no
// organisation allows it, so a policy the first organisation could run is never blocked; one that only some
// organisations allow stays open and is hinted with the organisations that refuse it. Pure: no React, no fetch.
import { adoOrgLabel } from "./ado-pat-display";
import type { SCMAccess } from "./types";

export interface OrgCeiling {
  /** The organisation as people name it. */
  org: string;
  ceiling: readonly string[];
}

/** The rows that name a ceiling, in server order. A row without one says nothing. */
export function orgCeilings(rows: readonly SCMAccess[]): OrgCeiling[] {
  return rows.flatMap((r) => (r.capability_ceiling ? [{ org: adoOrgLabel(r.org ?? ""), ceiling: r.capability_ceiling }] : []));
}

/** Every capability any organisation allows; undefined when no row names a ceiling (nothing locks). */
export function unionCeiling(cs: readonly OrgCeiling[]): string[] | undefined {
  if (cs.length === 0) return undefined;
  return [...new Set(cs.flatMap((c) => c.ceiling))];
}

/** The organisations that refuse `cap`, for a capability another organisation allows. Empty with one
 *  organisation, or when all of them refuse it (that is the lock, not a hint). */
export function refusedBy(cs: readonly OrgCeiling[], cap: string): string[] {
  if (cs.length < 2) return [];
  const refusing = cs.filter((c) => !c.ceiling.includes(cap)).map((c) => c.org);
  return refusing.length === cs.length ? [] : refusing;
}
