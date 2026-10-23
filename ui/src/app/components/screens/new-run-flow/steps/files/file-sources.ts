/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Add dialog chooses from: the repositories and folders of the
// person's workspaces, and their drive catalogue (GET /me/drives, read for the
// pool the run lands in). Nothing is guessed: a catalogue that cannot be read
// says so with the drive sentences the console already has.

import * as React from "react";
import { myDrives, type MeDrive, type MeDriveCatalogue, type MemberDriveCreate, type MemberDriveCreateReason } from "../../../../../lib/api/me-drives";
import { MEMBER } from "../../../../../lib/governance-copy";
import { DRIVE_MEMBER } from "../../../../../lib/user-drives-copy";
import { mountsFromInSentence } from "../../../../../lib/user-drives-display";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";

const UNAVAILABLE_TEXT: Record<string, string | undefined> = {
  groups_snapshot_stale: MEMBER.DENIED_STALE_GROUPS,
  governance_unavailable: DRIVE_MEMBER.NR_GOVERNANCE_UNAVAILABLE,
  unmountable: DRIVE_MEMBER.NR_UNAVAILABLE,
  drive_subject_unsupported: DRIVE_MEMBER.NR_UNAVAILABLE,
  unavailable: DRIVE_MEMBER.NR_UNAVAILABLE,
};

const DRIVE_REASON: Record<Exclude<NonNullable<MeDrive["unavailable_reason"]>, "bound_elsewhere">, string> = {
  paused: DRIVE_MEMBER.NR_PAUSED,
  unmountable: DRIVE_MEMBER.NR_UNAVAILABLE,
  placement_denied: NR_RUN.REASON_NOT_ALLOWED_PROFILE,
  not_in_pool: NR_FILES.NOT_IN_POOL,
};

/**
 * Why one drive cannot be attached; absent when it can. An organisation drive the pool's runners cannot mount says so
 * by name (M10): it mounts only from the organisation's runners, or it is bound to a runner this pool does not hold.
 */
export const driveReason = (d: MeDrive): string | undefined => {
  if (d.unavailable_reason === "bound_elsewhere") return DRIVE_MEMBER.NR_BOUND_ELSEWHERE(d.name);
  if (d.unavailable_reason === "not_in_pool" && d.source !== "personal") return DRIVE_MEMBER.NR_ORG_RUNNERS_ONLY(d.name);
  return d.unavailable_reason ? DRIVE_REASON[d.unavailable_reason] : undefined;
};

/** Where an attachable drive mounts from (M10): the runner and path in this pool, else the server's phrase. */
export const driveMountsLine = (d: MeDrive): string | undefined => {
  if (d.unavailable_reason) return undefined;
  if (d.mounted_from) return DRIVE_MEMBER.NR_MOUNTED_FROM(d.mounted_from.runner, d.mounted_from.path);
  return d.mounts_from ? DRIVE_MEMBER.NR_MOUNTS_FROM(mountsFromInSentence(d.mounts_from)) : undefined;
};

/**
 * The sentence beside a closed door: the canon string for each reason of the server's closed set. `disabled` is the
 * switch being off, so the arm shows no create control for it. The two reasons with no CREATE_* string show the
 * step's existing sentence for the same cause.
 */
const REASON_TEXT: Record<MemberDriveCreateReason, ((c: MemberDriveCreate) => string | undefined) | undefined> = {
  disabled: undefined,
  denied_by_unit: (c) => (c.denied_by_unit ? NR_FILES.CREATE_DENIED(c.denied_by_unit) : undefined),
  limit_reached: (c) => NR_FILES.CREATE_LIMIT(c.count, c.limit),
  unsupported: () => NR_FILES.CREATE_UNSUPPORTED,
  delete_unavailable: () => NR_FILES.CREATE_NO_DELETE,
  governance_unavailable: () => DRIVE_MEMBER.NR_GOVERNANCE_UNAVAILABLE,
  groups_snapshot_stale: () => MEMBER.DENIED_STALE_GROUPS,
};

/** The sentence for why a person cannot create a drive now; absent when the reason has none to show. */
export const createReasonText = (c: MemberDriveCreate): string | undefined => (c.reason ? REASON_TEXT[c.reason]?.(c) : undefined);

export interface DriveCatalogue {
  catalogue: MeDriveCatalogue | null;
  /** Why no drive can be attached at all: the door, then a catalogue that could not answer. */
  reason?: string;
  reload: () => void;
}

/** The person's drive catalogue for a pool; re-read when the pool changes. `enabled: false` reads nothing. */
export function useDriveCatalogue(poolId: string | undefined, enabled = true): DriveCatalogue {
  const [state, setState] = React.useState<{ catalogue: MeDriveCatalogue | null; failed: boolean }>({ catalogue: null, failed: false });
  const [generation, setGeneration] = React.useState(0);
  React.useEffect(() => {
    if (!enabled) return;
    let current = true;
    myDrives(poolId).then(
      (catalogue) => current && setState({ catalogue, failed: false }),
      () => current && setState({ catalogue: null, failed: true }),
    );
    return () => { current = false; };
  }, [poolId, generation, enabled]);
  const c = state.catalogue;
  const reason = state.failed
    ? DRIVE_MEMBER.NR_UNAVAILABLE
    : c?.denied_by_unit
      ? DRIVE_MEMBER.NR_DENIED(c.denied_by_unit)
      : c?.unavailable ? UNAVAILABLE_TEXT[c.unavailable] ?? DRIVE_MEMBER.NR_UNAVAILABLE : undefined;
  const reload = React.useCallback(() => setGeneration((g) => g + 1), []);
  return { catalogue: c, reason, reload };
}
