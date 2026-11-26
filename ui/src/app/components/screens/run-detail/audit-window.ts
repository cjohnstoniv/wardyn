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

/** A run's trail as the run page holds it: oldest first, with `gap` true when rows between its oldest and newest pages
 * were left out. */
export type AuditTrail = { events: AuditEvent[]; gap: boolean };

/** One read of the trail. `added` is the rows new to the trail when the read was a newest-window read, and null when
 * it read the trail whole. */
export type TrailRead = { trail: AuditTrail; added: AuditEvent[] | null };

/** The newest rows a poll reads. A window that reaches a row the trail holds holds every row written since. */
export const NEWEST_POLL_ROWS = 100;

/** New rows joined on the trail's end. Past twice LIST_LIMIT the trail keeps its oldest LIST_LIMIT rows and its newest
 * LIST_LIMIT, as a first read does, and marks the gap. */
export function joinRows(t: AuditTrail, rows: AuditEvent[]): AuditTrail {
  const seen = new Set(t.events.map((e) => e.id));
  const events = [...t.events, ...rows.filter((e) => !seen.has(e.id))];
  if (events.length <= 2 * LIST_LIMIT) return { events, gap: t.gap };
  return { events: [...events.slice(0, LIST_LIMIT), ...events.slice(-LIST_LIMIT)], gap: true };
}

/**
 * Reads a run's trail. With a trail held, it reads the newest NEWEST_POLL_ROWS rows: the trail is append-only and the
 * log commits rows in position order, so a window that holds a row the trail holds also holds every row written
 * since, and only those are joined on. Otherwise (a first read, an empty trail, or a window of rows all new) it reads
 * as before: the oldest page, kept from the trail once full, and past it the newest page.
 */
export async function readTrail(runId: string, held: AuditTrail | null): Promise<TrailRead> {
  if (held && held.events.length > 0) {
    const window = await audit.listAudit(runId, { newest: true, limit: NEWEST_POLL_ROWS });
    const seen = new Set(held.events.map((e) => e.id));
    const fresh = window.filter((e) => !seen.has(e.id));
    if (fresh.length < window.length) return { trail: joinRows(held, fresh), added: fresh };
  }
  const oldest = held && held.events.length >= LIST_LIMIT ? held.events.slice(0, LIST_LIMIT) : null;
  return { trail: await withNewestEvents(runId, oldest ?? (await audit.listAudit(runId))), added: null };
}

/** Whether rows hold a row of the run page's recording index families: its three filtered reads match only these. */
export function touchesRecordingIndex(rows: AuditEvent[]): boolean {
  return rows.some((e) => e.action.startsWith("session.recording") || e.action.startsWith("desktop.recording"));
}
