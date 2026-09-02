/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// User drives (0.7) — the six operatorOnly routes behind the /drives screen.
// Mirrors internal/api/user_drives.go + internal/types/user_drive.go; every
// route is under /api/v1 via wfetch.
//
// The wire types live HERE, beside the one consumer, for the reason
// lib/api/governance.ts states for its own: a second module holding interfaces
// nothing else imports is a file to keep in sync for no reader. They are a
// HAND-MAINTAINED MIRROR — every field name below is the Go json tag verbatim,
// and a removed wire field is a runtime TypeError the e2e lane catches, not a
// compile error here.
import type { CapabilitySubjectType } from "../types";
import { asJson, errText, HttpError, unwrapList, wfetch } from "./core";

// types.DriveBackend — four backends, two per runner.
export type DriveBackend = "docker_volume" | "host_path" | "k8s_pvc" | "k8s_pvc_static";
// types.HomeTemplate. There is deliberately NO whole-email template: an address
// carries an "@", which no directory segment (and no DNS-1123 label) can hold.
export type HomeTemplate = "hash" | "sub" | "email_local";
// types.DriveReclaim — a DECLARED INTENT the admin carries out by command.
export type DriveReclaim = "retain" | "delete";
// types.StorageEnforcement — what actually binds bytes. `filesystem` is in the
// vocabulary for a value no v1 backend yields (prompt §2.7).
export type StorageEnforcement = "filesystem" | "request" | "external" | "none";

// types.UserDrive — one admin-registered drive (migration 0054's row).
export interface UserDrive {
  id: string;
  name: string;
  backend: DriveBackend;
  host_root?: string;
  storage_class?: string;
  home_template: HomeTemplate;
  size_mib?: number;
  writable?: boolean;
  reclaim: DriveReclaim;
  created_at: string;
  updated_at: string;
  created_by?: string;
}

// types.UserDriveListItem — the row plus the count the delete dialog pre-fills
// from. The count is knowable ONLY here; the server's own 409 carries none.
export interface UserDriveListItem extends UserDrive {
  grant_count: number;
}

// types.UserDriveGrant — one subject bound to one drive, unique per subject.
//
// writable_override is a *bool on the wire: absent means "same as the drive",
// which is a THIRD state neither `true` nor `false` can carry.
export interface UserDriveGrant {
  id: string;
  subject_type: CapabilitySubjectType;
  subject: string;
  drive_id: string;
  priority: number;
  size_mib_override?: number;
  writable_override?: boolean | null;
  home_override?: string;
  enabled: boolean;
  created_at: string;
  created_by?: string;
}

// GET /drives's body: the whole screen in one read, plus the two DEPLOYMENT
// facts the console cannot derive — whether WARDYN_USER_DRIVE_HOST_ROOTS is set
// at all (a boolean, never the roots themselves) and which substrate this
// deployment dispatches to, so the backend picker offers the pair that can
// actually mount here.
export interface UserDrivesSnapshot {
  drives: UserDriveListItem[];
  grants: UserDriveGrant[];
  host_roots_configured: boolean;
  runner_target: string;
}

// userDriveRequest — the POST/PUT body. id/created_at/updated_at/created_by are
// never sent; the server assigns provenance.
export interface UserDriveInput {
  name: string;
  backend: DriveBackend;
  host_root?: string;
  storage_class?: string;
  home_template?: HomeTemplate;
  size_mib?: number;
  writable?: boolean;
  reclaim?: DriveReclaim;
}

// userDriveGrantRequest. `enabled` and `writable_override` are pointers on the
// Go side, so an omitted key is not the same as `false` — the caller omits
// rather than sends a default it did not mean.
export interface UserDriveGrantInput {
  subject_type: CapabilitySubjectType;
  subject: string;
  drive_id: string;
  priority: number;
  size_mib_override?: number;
  writable_override?: boolean;
  home_override?: string;
  enabled?: boolean;
}

// The POST /drives/preview body — the same two claim lists the governance
// preview takes, because the server folds them with the same normalizer.
export interface UserDrivePreviewInput {
  user_subjects: string[];
  groups: string[];
}

// userDrivePreviewResponse: which drive a principal carrying those claims would
// mount, and the exact OBJECT NAME an offboarding command needs.
//
// EVERY field is `omitempty`, so an EMPTY OBJECT is the answer for "no
// allocation matched". A paused winner answers with the drive, the tier and the
// folded size/mode but NO home or object name: nothing is derived above a
// paused row, because nothing would mount.
export interface UserDrivePreview {
  drive_name?: string;
  matched_tier?: CapabilitySubjectType;
  home_name?: string;
  object_name?: string;
  size_mib?: number;
  writable?: boolean;
  enforcement?: StorageEnforcement;
  paused?: boolean;
}

// DriveBackend.Kind() — managed (Wardyn allocates the object) vs share (it
// exists already and Wardyn only binds it). Derived in Go from the backend and
// NEVER stored, so it is not on the wire and the console derives it too. An
// unknown backend reads as a share, the conservative half, exactly as Go does.
export const isManagedBackend = (b: DriveBackend): boolean =>
  b === "docker_volume" || b === "k8s_pvc";

// types.EnforcementFor — what binds this backend's bytes. Also derived in Go
// and absent from the list wire type (the preview and /me carry the resolved
// value; a table row does not). An unknown backend reads as `none`: claiming
// enforcement for a row this build does not understand is the one answer that
// could mislead an admin into trusting a cap.
export function enforcementFor(b: DriveBackend): StorageEnforcement {
  if (b === "k8s_pvc") return "request";
  if (b === "host_path" || b === "k8s_pvc_static") return "external";
  return "none";
}

// DriveBackend.RunnerTarget(), inverted: the two backends this deployment's
// runner can mount. An unrecognised runner_target offers none rather than
// guessing a pair whose every save would 400.
export function backendsFor(runner: string): DriveBackend[] {
  if (runner === "docker") return ["docker_volume", "host_path"];
  if (runner === "k8s") return ["k8s_pvc", "k8s_pvc_static"];
  return [];
}

export const drives = {
  // GET /api/v1/drives -> the whole picture. Nil Go slices encode as null, so
  // both lists are coerced and every caller can map over them.
  async getDrives(): Promise<UserDrivesSnapshot> {
    const res = await wfetch("/drives", { method: "GET" });
    const body = await asJson<Partial<UserDrivesSnapshot>>(res);
    return {
      drives: unwrapList<UserDriveListItem>(body.drives),
      grants: unwrapList<UserDriveGrant>(body.grants),
      host_roots_configured: !!body.host_roots_configured,
      runner_target: body.runner_target ?? "",
    };
  },

  // POST /api/v1/drives -> 201. The 400s are composed by validateUserDrive and
  // reach the caller as an HttpError carrying the server's own message: the
  // host root outside the roots, the denied prefix, the backend/runner
  // mismatch, `hash` on a share, and a zero size on a k8s_pvc claim.
  async createDrive(input: UserDriveInput): Promise<UserDrive> {
    return asJson<UserDrive>(await wfetch("/drives", { method: "POST", body: JSON.stringify(input) }));
  },

  // PUT /api/v1/drives/{id} -> 200. Rename included: ON DELETE RESTRICT makes
  // delete-and-recreate impossible for an allocated drive.
  async updateDrive(id: string, input: UserDriveInput): Promise<UserDrive> {
    const res = await wfetch(`/drives/${encodeURIComponent(id)}`, { method: "PUT", body: JSON.stringify(input) });
    return asJson<UserDrive>(res);
  },

  // DELETE /api/v1/drives/{id} -> 204.
  //
  // The 409 MUST reach the caller: it is the ON DELETE RESTRICT refusal and it
  // is authoritative for the race the console's own grant_count cannot see. A
  // 404 is tolerated as "already gone" — the row is absent either way.
  async deleteDrive(id: string): Promise<void> {
    const res = await wfetch(`/drives/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) throw new HttpError(res.status, await errText(res));
  },

  // POST /api/v1/drives/grants -> the stored row, 201 for a genuinely new
  // allocation and 200 when an existing one was REPOINTED (the store returns
  // the existing row's id on conflict). The console renders ALLOC_REPLACED on
  // the 200, so the status is surfaced rather than swallowed.
  async upsertGrant(input: UserDriveGrantInput): Promise<{ grant: UserDriveGrant; replaced: boolean }> {
    const res = await wfetch("/drives/grants", { method: "POST", body: JSON.stringify(input) });
    const replaced = res.status === 200;
    return { grant: await asJson<UserDriveGrant>(res), replaced };
  },

  // DELETE /api/v1/drives/grants/{id} -> 204. Deletes no data: the directory
  // stays until the admin reclaims it.
  async deleteGrant(id: string): Promise<void> {
    const res = await wfetch(`/drives/grants/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) throw new HttpError(res.status, await errText(res));
  },

  // POST /api/v1/drives/preview -> 200, the resolved drive or {}.
  //
  // The dry run runs THE resolver server-side (the identical resolveUserDriveFor
  // the enforcement path takes), so there is deliberately no client-side
  // precedence here: a second implementation of the precedence rule is a second
  // implementation of the answer.
  async previewDrive(input: UserDrivePreviewInput): Promise<UserDrivePreview> {
    const res = await wfetch("/drives/preview", { method: "POST", body: JSON.stringify(input) });
    return asJson<UserDrivePreview>(res);
  },
};
