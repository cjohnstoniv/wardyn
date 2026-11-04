/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Review's lines for a run's machine (board KM-2), in Where it runs under the pool: the machine it keeps or resumes, and
// what that does. A machine the person's list no longer holds says nothing here; the server's check names it.

import { shortDate } from "../../../../lib/format";
import { gbOf, KEEP_REVIEW } from "../../../wardyn/copy/keep-machine";
import type { RunIntent } from "../draft/run-intent";
import { modeOf } from "../lenses/shape-lens";
import type { KeptMachinesRead } from "../steps/run/kept-machines-provider";

export interface KeptReviewLines {
  /** K-25: "Machine · {name}". */
  machine: string;
  /** K-20, K-22, K-23 or K-24. */
  detail: string;
}

export function keptReviewLines(intent: RunIntent, read: KeptMachinesRead, mac: string | undefined): KeptReviewLines | undefined {
  const asked = intent.machine;
  if (!asked || intent.machine_kind !== "macos_vm" || modeOf(intent) !== "interactive" || read.status !== "ready") return undefined;
  if (asked.keep) {
    return {
      machine: KEEP_REVIEW.MACHINE(asked.name?.trim() || KEEP_REVIEW.NEW_UNNAMED),
      detail: KEEP_REVIEW.KEEP(mac ?? "", gbOf(read.data.worst_bytes)),
    };
  }
  const m = read.data.machines.find((k) => k.id === asked.resume);
  if (!m) return undefined;
  const on = m.runner_name || mac || "its Mac";
  const detail = asked.fresh ? KEEP_REVIEW.FRESH(m.name, on)
    : m.scrubbed ? KEEP_REVIEW.RESUME(m.name, shortDate(m.last_used_at))
    : KEEP_REVIEW.RESUME_NOT_CLEANED(m.name, shortDate(m.last_used_at));
  return { machine: KEEP_REVIEW.MACHINE(m.name), detail };
}
