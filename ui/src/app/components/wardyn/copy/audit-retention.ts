/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Audit → Retention tab and its drop dialog: mock packet M4 (approved
// 2026-10-03), the strings character for character. GET /audit/retention,
// POST /audit/retention/drop and GET /audit/export?partition= are the
// security tier's (internal/api/audit_retention.go).
//
// The server's refusal reasons are keyed here WITHOUT their audit_retention_
// prefix; inside_window is the wire name of what the copy calls
// inside_retention (reasonKey below does both).

/** `audit_retention_inside_window` -> `inside_retention`; every other reason loses only its prefix. */
export function reasonKey(reason: string): string {
  const k = reason.replace(/^audit_retention_/, "");
  return k === "inside_window" ? "inside_retention" : k;
}

export const RETENTION = {
  TAB_EVENTS: "Events",
  TAB: "Retention",
  KEPT_FOR: "Kept for",
  FOREVER: "Forever",
  DAYS: (n: number) => `${n} days`,
  PENDING: (n: number, date: string) => `Changes to ${n} days on ${date}.`,
  SOURCE:
    "Set by WARDYN_AUDIT_RETENTION_DAYS on the server. A shorter period takes effect 30 days after the restart that sets it, so no one can shorten it and drop history the same day.",
  CUTOVER: (date: string) =>
    `Events before ${date} are in one partition, because Wardyn started splitting the log by month that day.`,
  AHEAD: (n: number) => `${n} months of partitions ready`,
  AHEAD_LOW: (n: number) =>
    `Only ${n} months of partitions are ready. Wardyn adds them daily; if this stays low, check the server log.`,
  COL_PARTITION: "Partition",
  COL_COVERS: "Covers",
  COL_EVENTS: "Events",
  COL_STATE: "State",
  COL_DROP: "Drop",
  COVERS_LEGACY: (date: string) => `Before ${date}`,
  OPEN: "Open",
  CLOSED: "Closed",
  ELIGIBLE: "Can be dropped",
  WHY: {
    not_oldest: "An older partition comes first",
    not_closed: "Still open",
    inside_retention: "Inside the retention period",
    live_run: "Holds events of a live run",
  } as Record<string, string>,
  EXPORT: "Export",
  DROP: "Drop",
  FOOTER:
    "Dropping deletes a whole partition for good. Each drop is recorded in the Audit log with its digest, and verification starts from it.",
} as const;

export const DROP = {
  TITLE: (month: string) => `Drop the ${month} partition?`,
  TITLE_LEGACY: (date: string) => `Drop every event before ${date}?`,
  BODY: (n: string) =>
    `This deletes its ${n} events for good. Export it first and keep the archive: after the drop, it is the only copy.`,
  READABLE: "Readable export",
  RAW: "Raw archive",
  RAW_HINT: "The raw archive lets anyone recompute every row hash and the digest without Wardyn.",
  DIGEST: "Digest from the archive's footer",
  DIGEST_HINT:
    "Paste it from the archive you kept. Wardyn recomputes it and refuses the drop if they differ.",
  CONFIRM: "Drop partition",
  DONE: (n: string) => `Dropped ${n} events.`,
  DONE_AUDIT: "Recorded in the Audit log as audit.retention.partition_dropped.",
  REFUSED: {
    digest_mismatch: "The digest doesn't match this partition. Check you pasted it from this partition's archive.",
    not_oldest: "Only the oldest partition can be dropped.",
    not_closed: "This partition is still receiving events.",
    inside_retention: "This partition is still inside the retention period.",
    live_run: "This partition holds events of a run that is still live. Drop it after the run ends.",
  } as Record<string, string>,
  FAILED: "The drop didn't finish, or its answer was lost. Reload: if the partition is still listed, nothing was deleted.",
} as const;
