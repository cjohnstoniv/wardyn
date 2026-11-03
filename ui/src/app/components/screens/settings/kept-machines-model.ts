/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What a kept machine's row says, from the list wire alone (board KM-4): its chip, its one line of news and whether it
// may be deleted. A machine a run holds is never deletable; one being deleted is not either, as the delete has started.

import { shortDate } from "../../../lib/format";
import type { KeptMachine } from "../../../lib/types";
import { KEEP_LIST } from "../../wardyn/copy/keep-machine";

const DAY_MS = 24 * 60 * 60 * 1000;
/** A machine this close to its expiry says so (K-58). */
export const EXPIRES_SOON_DAYS = 7;

/** The name of the Mac a machine is kept on, as the server sent it. */
export const macOf = (m: Pick<KeptMachine, "runner_name">): string => m.runner_name || KEEP_LIST.THIS_MAC;

export interface KeptRow {
  chip: string;
  tone: "success" | "info" | "warning" | "neutral";
  /** K-56 to K-60, whichever applies; absent for a plain kept machine. */
  line?: string;
  canDelete: boolean;
}

/** `runLabel` is the holder run's title, else its short id (K-56's {run}); `now` is the clock, for K-58. */
export function rowOf(m: KeptMachine, runLabel: string, now: number): KeptRow {
  if (m.state === "deleting") {
    // delete_reason says an expiry deleted it: Expired, not Deleting (K-59).
    if (m.delete_reason === "expired") {
      return { chip: KEEP_LIST.CHIP_EXPIRED, tone: "neutral", line: KEEP_LIST.EXPIRED(shortDate(m.delete_started_at ?? m.expires_at), macOf(m)), canDelete: false };
    }
    return { chip: KEEP_LIST.CHIP_DELETING, tone: "warning", line: KEEP_LIST.DELETE_UNCONFIRMED, canDelete: false };
  }
  if (m.state === "creating") return { chip: KEEP_LIST.CHIP_PREPARING, tone: "info", line: KEEP_LIST.PREPARING(runLabel), canDelete: false };
  if (m.state === "in_use") return { chip: KEEP_LIST.CHIP_IN_USE, tone: "info", line: KEEP_LIST.IN_USE(runLabel), canDelete: false };
  if (!m.scrubbed) return { chip: KEEP_LIST.CHIP_NOT_CLEANED, tone: "warning", line: KEEP_LIST.NOT_SCRUBBED, canDelete: true };
  const days = Math.ceil((Date.parse(m.expires_at) - now) / DAY_MS);
  if (days <= EXPIRES_SOON_DAYS) return { chip: KEEP_LIST.CHIP_KEPT, tone: "success", line: KEEP_LIST.EXPIRES_SOON(Math.max(days, 1)), canDelete: true };
  return { chip: KEEP_LIST.CHIP_KEPT, tone: "success", canDelete: true };
}

/** The Macs the machines are kept on, each once, in the order they first appear. */
export const macsOf = (machines: readonly KeptMachine[]): string[] => [...new Set(machines.map(macOf))];
