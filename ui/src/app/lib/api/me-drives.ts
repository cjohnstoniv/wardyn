/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GET /api/v1/me/drives — the drive catalogue New Run attaches from. Mirrors
// pkg/client/me_drives.go (wire-parity.test.ts pins the keys). Nothing here
// names a host path, a storage object or another person.
import type { DriveBackend, StorageEnforcement } from "./drives";
import { asJson, errText, HttpError, unwrapList, wfetch } from "./core";

export type MeDriveSection = "user" | "shared";
export type MeDriveSource = "personal" | "team" | "organisation";
export type MeDriveUnavailableReason = "paused" | "unmountable" | "placement_denied" | "not_in_pool";

/** One drive of the caller's catalogue (pkg/client.MeDrive). */
export interface MeDrive {
  id: string;
  name: string;
  section: MeDriveSection;
  source: MeDriveSource;
  backend: DriveBackend;
  /** Where the drive mounts, derived by the server. */
  target: string;
  writable: boolean;
  size_mib?: number;
  enforcement: StorageEnforcement;
  /** The drive `drive.enabled` mounts at /home/agent/drive. */
  legacy: boolean;
  unavailable_reason?: MeDriveUnavailableReason;
  /** The caller created it; only an owned drive takes DELETE /me/drives/{id}. */
  owned: boolean;
}

/** GET /me/drives (pkg/client.MeDriveCatalogue). */
export interface MeDriveCatalogue {
  drives: MeDrive[];
  denied_by_profile?: string;
  unavailable?: string;
  pool_checked: boolean;
  /** Whether the caller may create a drive now; absent when the caller is not a person. */
  create?: MemberDriveCreate;
}

/** Why a person cannot create a drive now (pkg/client.MemberDriveCreateReason). */
export type MemberDriveCreateReason =
  | "disabled"
  | "denied_by_profile"
  | "limit_reached"
  | "unsupported"
  | "delete_unavailable"
  | "governance_unavailable"
  | "groups_snapshot_stale";

/** The catalogue's create availability (pkg/client.MemberDriveCreate). */
export interface MemberDriveCreate {
  allowed: boolean;
  /** Set exactly when allowed is false. */
  reason?: MemberDriveCreateReason;
  denied_by_profile?: string;
  /** The caller's drives, one being deleted included. */
  count: number;
  /** Drives per person; 0 when the switch is off. */
  limit: number;
  /** Whether a run may keep its work folder as a drive (the keep_main_drive switch). */
  keep_work_folder: boolean;
  /** What binds a new drive's size here; "none" on Docker, whose volumes have no size cap. */
  enforcement?: StorageEnforcement;
}

/** Reads the caller's drive catalogue, checked against runnerPoolId when given. */
export async function myDrives(runnerPoolId?: string): Promise<MeDriveCatalogue> {
  const qs = runnerPoolId ? `?runner_pool_id=${encodeURIComponent(runnerPoolId)}` : "";
  const body = await asJson<MeDriveCatalogue>(await wfetch(`/me/drives${qs}`, { method: "GET" }));
  return { ...body, drives: unwrapList<MeDrive>(body.drives), pool_checked: !!body.pool_checked };
}

/** POST /me/drives (pkg/client.MemberDriveCreateRequest): the caller's own new drive. */
export async function createMyDrive(name: string): Promise<MeDrive> {
  return asJson<MeDrive>(await wfetch("/me/drives", { method: "POST", asPerson: true, body: JSON.stringify({ name }) }));
}

/** DELETE /me/drives/{id}: deletes the caller's own drive and its files. */
export async function deleteMyDrive(id: string): Promise<void> {
  const res = await wfetch(`/me/drives/${encodeURIComponent(id)}`, { method: "DELETE" });
  if (!res.ok) throw new HttpError(res.status, await errText(res));
}
