/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Permissioning CRUD (0.6 pillar 2) — the four operatorOnly routes behind the
// admin Permissions screen, plus the member-safe read of the caller's OWN
// effective set. Mirrors internal/api/permissions.go; every route is under
// /api/v1 via wfetch.
import type {
  AccessUserType,
  AvailabilityView,
  CapabilityGrant,
  CapabilityGrantInput,
  MeCapabilities,
  PermissionsSnapshot,
} from "../types";
import { asJson, errText, HttpError, unwrapList, wfetch } from "./core";

// {kind}/{value} — the server takes the value as the REST of the path (an
// image ref carries slashes, permissions_availability.go's availabilityTarget),
// so each segment is encoded on its own rather than encodeURIComponent-ing the
// whole value, which would turn a real "/" into "%2F" and 400 as a different
// value than the one on screen. The characters a Go path never escapes
// ($&+,:;=@) are sent bare: escaped, they set URL.RawPath, the router hands the
// handler "%3A" instead of ":", and an image ref's tag reads as another value.
function availabilityPath(kind: string, value: string): string {
  const encodedValue = value
    .split("/")
    .map((s) => encodeURIComponent(s).replace(/%(24|26|2B|2C|3A|3B|3D|40)/gi, (e) => decodeURIComponent(e)))
    .join("/");
  return `/permissions/availability/${encodeURIComponent(kind)}/${encodedValue}`;
}

// What an upsert actually did. The server distinguishes a genuinely new row
// (201) from a re-grant that flipped an existing row's effect in place (200) —
// that status is the ONLY signal for it, and it is what drives the screen's
// PERM.DUPLICATE line, so it is surfaced rather than thrown away.
export interface GrantUpsert {
  grant: CapabilityGrant;
  updated: boolean;
}

export const permissions = {
  // GET /api/v1/permissions -> {grants, enforcement}. A nil Go slice/map
  // encodes as null, so both halves are coerced here and every caller can map
  // over grants / index enforcement without a guard.
  async getPermissions(): Promise<PermissionsSnapshot> {
    const res = await wfetch("/permissions", { method: "GET" });
    const body = await asJson<Partial<PermissionsSnapshot>>(res);
    return {
      grants: unwrapList<CapabilityGrant>(body.grants),
      enforcement: body.enforcement ?? {},
    };
  },

  // POST /api/v1/permissions/grants -> the stored row. 201 = created,
  // 200 = an existing (subject_type, subject, capability, value) had its
  // effect updated in place.
  async upsertGrant(input: CapabilityGrantInput): Promise<GrantUpsert> {
    const res = await wfetch("/permissions/grants", {
      method: "POST",
      body: JSON.stringify(input),
    });
    const updated = res.status === 200;
    return { grant: await asJson<CapabilityGrant>(res), updated };
  },

  // DELETE /api/v1/permissions/grants/{id} -> 204. A 404 is tolerated as
  // "already gone" (same shape as ssh-keys.deleteKey): the row is absent
  // either way, which is what the caller asked for.
  async deleteGrant(id: string): Promise<void> {
    const res = await wfetch(`/permissions/grants/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) {
      throw new HttpError(res.status, await errText(res));
    }
  },

  // PUT /api/v1/permissions/enforcement -> the saved map. FULL REPLACE: a key
  // omitted from the body is a real "stop enforcing that kind", so callers
  // must send the whole map, never a patch.
  async putEnforcement(enforcement: Record<string, boolean>): Promise<Record<string, boolean>> {
    const res = await wfetch("/permissions/enforcement", {
      method: "PUT",
      body: JSON.stringify(enforcement),
    });
    return (await asJson<Record<string, boolean> | null>(res)) ?? {};
  },

  // GET /api/v1/permissions/availability/{kind}/{value} -> one resource's
  // "Available to" state (the restricted bit + who is named). securityOps.
  async getAvailability(kind: string, value: string): Promise<AvailabilityView> {
    const res = await wfetch(availabilityPath(kind, value), { method: "GET" });
    return asJson<AvailabilityView>(res);
  },

  // GET /api/v1/user-types -> every user type (securityOps), so an audience
  // chip can name a type rather than show its id.
  async listUserTypes(): Promise<AccessUserType[]> {
    const res = await wfetch("/user-types", { method: "GET" });
    return unwrapList<AccessUserType>((await asJson<{ user_types?: unknown }>(res)).user_types);
  },

  // PUT /api/v1/permissions/availability/{kind}/{value} -> the same view, bit
  // flipped. Turning "Only…" on with nobody already listed is refused (400);
  // the server's message is surfaced verbatim by the caller, never reworded.
  async putAvailability(kind: string, value: string, restricted: boolean): Promise<AvailabilityView> {
    const res = await wfetch(availabilityPath(kind, value), {
      method: "PUT",
      body: JSON.stringify({ restricted }),
    });
    return asJson<AvailabilityView>(res);
  },

  // GET /api/v1/me/capabilities -> the caller's own grants + the enforcement
  // map + their group snapshot. Member-safe (not operatorOnly), which is what
  // makes the why-denied surfaces work for the people they are written for.
  async getMyCapabilities(): Promise<MeCapabilities> {
    const res = await wfetch("/me/capabilities", { method: "GET" });
    const body = await asJson<Partial<MeCapabilities>>(res);
    return {
      grants: unwrapList<CapabilityGrant>(body.grants),
      enforcement: body.enforcement ?? {},
      session_groups: unwrapList<string>(body.session_groups),
      groups_snapshot_stale: !!body.groups_snapshot_stale,
    };
  },
};
