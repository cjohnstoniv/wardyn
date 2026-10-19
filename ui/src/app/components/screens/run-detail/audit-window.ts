/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { audit } from "../../../lib/api/audit";
import { LIST_LIMIT } from "../../../lib/api/core";
import type { AuditEvent } from "../../../lib/types";

/**
 * A run's trail past the oldest-first page cap: when that page comes back full, its newest page is joined on so the
 * latest events (new egress, denies, mints) are not cut off. `gap` is true when the two pages do not meet, so rows
 * between them may be missing and counts read off the result are a floor.
 */
export async function withNewestEvents(runId: string, head: AuditEvent[]): Promise<{ events: AuditEvent[]; gap: boolean }> {
  if (head.length < LIST_LIMIT) return { events: head, gap: false };
  try {
    const tail = await audit.listAudit(runId, { newest: true });
    const seen = new Set(head.map((e) => e.id));
    const fresh = tail.filter((e) => !seen.has(e.id));
    return { events: [...head, ...fresh], gap: tail.length > 0 && fresh.length === tail.length };
  } catch {
    return { events: head, gap: true };
  }
}
