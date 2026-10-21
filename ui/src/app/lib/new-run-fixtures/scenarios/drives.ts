/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { MeUserDrive } from "../../api/health";
import type { MeDrive, MeDriveCatalogue, MemberDriveCreate, MemberDriveCreateReason } from "../../api/me-drives";
import { everyScenario, type ByScenario } from "./index";

/** GET /me's `user_drive`: the person's one resolved drive, writable. */
export const LEGACY_USER_DRIVE: MeUserDrive = { name: "Scratch", backend: "docker_volume", size_mib: 10240, writable: true, enforcement: "none" };

/** The same drive as an admin set it read-only. */
export const LEGACY_USER_DRIVE_READ_ONLY: MeUserDrive = { ...LEGACY_USER_DRIVE, writable: false };

const drive = (over: Partial<MeDrive> & Pick<MeDrive, "id" | "name">): MeDrive => ({
  section: "user", source: "personal", backend: "docker_volume", target: `/home/agent/drives/${over.id}`, writable: true,
  enforcement: "none", legacy: false, owned: false, ...over,
});

/** The person's legacy drive: the one `/home/agent/drive` holds. */
export const DRIVE_LEGACY = drive({ id: "d-legacy", name: "Scratch", target: "/home/agent/drive", legacy: true, size_mib: 10240 });
/** A drive the person created: the only kind that takes a delete. */
export const DRIVE_OWNED = drive({ id: "d-owned", name: "Investigation files", owned: true });
/** An admin-granted team drive on a host folder, read-only for this person. */
export const DRIVE_LOCKED = drive({ id: "d-team", name: "Team reference", source: "team", backend: "host_path", writable: false, target: "/home/agent/drives/team-reference" });
export const DRIVE_NOT_IN_POOL = drive({ id: "d-remote", name: "Cluster scratch", source: "organisation", backend: "k8s_pvc", unavailable_reason: "not_in_pool" });
export const DRIVE_SHARED = drive({
  id: "d-share", name: "Archive", section: "shared", source: "organisation", backend: "host_path", writable: true, target: "/home/agent/shared/archive",
});

export const CREATE_ALLOWED: MemberDriveCreate = { allowed: true, count: 1, limit: 3, keep_work_folder: false };

/** One closed door per reason, so every disabled line is drawn. */
const REASONS: Record<MemberDriveCreateReason, true> = {
  disabled: true, denied_by_profile: true, limit_reached: true, unsupported: true, delete_unavailable: true, governance_unavailable: true, groups_snapshot_stale: true,
};
export const CREATE_DOORS: Record<MemberDriveCreateReason, MemberDriveCreate> = Object.fromEntries(
  (Object.keys(REASONS) as MemberDriveCreateReason[]).map((reason) => [reason, {
    allowed: false, reason, keep_work_folder: false, count: reason === "limit_reached" ? 3 : 0, limit: reason === "limit_reached" ? 3 : 0,
    ...(reason === "denied_by_profile" ? { denied_by_profile: "Contractors" } : {}),
  }]),
) as Record<MemberDriveCreateReason, MemberDriveCreate>;

const catalogue = (drives: MeDrive[], create?: MemberDriveCreate): MeDriveCatalogue => ({ drives, pool_checked: true, ...(create ? { create } : {}) });

/** GET /me/drives?runner_pool_id=, per scenario. Person-created drives are off unless a scenario says so. */
export const SCENARIO_DRIVE_CATALOGUE: ByScenario<MeDriveCatalogue> = everyScenario(
  catalogue([DRIVE_LEGACY, DRIVE_OWNED, DRIVE_LOCKED, DRIVE_NOT_IN_POOL, DRIVE_SHARED], CREATE_ALLOWED),
  {
    governed: catalogue([DRIVE_LEGACY, DRIVE_LOCKED], CREATE_DOORS.denied_by_profile),
    offline: catalogue([DRIVE_LEGACY], CREATE_DOORS.disabled),
    admin: catalogue([DRIVE_OWNED], CREATE_ALLOWED),
  },
);
