/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared by the Retention tab and its drop dialog (mock packet M4): the date
// wording and the archive download.
import type { AuditRetentionPartition } from "../../lib/types";
import { audit as api, type PartitionExportForm } from "../../lib/api/audit";
import { RETENTION } from "../wardyn/copy/audit-retention";

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

// The log's months are UTC, so its dates read in UTC whatever the viewer's zone.
export function dayLabel(iso: string): string {
  const d = new Date(iso);
  return `${d.getUTCDate()} ${MONTHS[d.getUTCMonth()]} ${d.getUTCFullYear()}`;
}

export function monthLabel(iso: string): string {
  const d = new Date(iso);
  return `${MONTHS[d.getUTCMonth()]} ${d.getUTCFullYear()}`;
}

/** The legacy partition ends where the log started splitting by month. */
export function legacyDate(p: AuditRetentionPartition, cutover: string): string {
  return dayLabel(p.hi ?? cutover);
}

/** A partition's "Covers" text: its month, or everything before the cutover for the legacy one. */
export function coversLabel(p: AuditRetentionPartition, cutover: string): string {
  return p.lo ? monthLabel(p.lo) : RETENTION.COVERS_LEGACY(legacyDate(p, cutover));
}

/** Fetches one partition's archive and hands it to the browser as a file. */
export async function downloadPartition(name: string, form: PartitionExportForm): Promise<void> {
  const blob = await api.exportPartition(name, form);
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = `${name}.${form}.ndjson`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}
