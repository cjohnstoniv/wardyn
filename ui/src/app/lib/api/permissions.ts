/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Permissioning CRUD (0.6 pillar 2) — the four operatorOnly routes behind the
// admin Permissions screen, plus the member-safe read of the caller's OWN
// effective set. Mirrors internal/api/permissions.go; every route is under
// /api/v1 via wfetch.
import type {
  CapabilityGrant,
  CapabilityGrantInput,
  CapabilitySubjectType,
  MeCapabilities,
  PermissionsSnapshot,
} from "../types";
import { asJson, errText, HttpError, unwrapList, wfetch } from "./core";

// The Explain grid (K4/AK-5, #739) — GET /permissions/explain's body. Each row
// is one (kind, value) cell of the type editor's "What this type gets" grid
// (user-types-design.md rev 4 §2.6). Mirrors internal/api/
// capabilities_explain.go's explainResponse/capExplainRow exactly.
export type ExplainState = "everyone" | "this_type" | "blocked" | "admins_only" | "not_available";

export interface ExplainRow {
  kind: string;
  value: string;
  state: ExplainState;
}

export interface ExplainResponse {
  subject_type: CapabilitySubjectType;
  subject: string;
  kinds_version: number;
  rows: ExplainRow[];
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

  // GET /api/v1/permissions/explain?subject_type=&subject=&kinds= -> the
  // Explain grid (#739) for one named subject — the User types screen asks
  // for subject_type=user_type. securityOps, same tier as the rest of
  // /permissions. `kinds` defaults to every kind in CAPABILITY_KINDS' order
  // when omitted.
  async explainCapabilities(
    subjectType: CapabilitySubjectType,
    subject: string,
    kinds?: string[],
  ): Promise<ExplainResponse> {
    const params = new URLSearchParams({ subject_type: subjectType, subject });
    if (kinds?.length) params.set("kinds", kinds.join(","));
    const res = await wfetch(`/permissions/explain?${params.toString()}`, { method: "GET" });
    const body = await asJson<Partial<ExplainResponse>>(res);
    return {
      subject_type: body.subject_type ?? subjectType,
      subject: body.subject ?? subject,
      kinds_version: body.kinds_version ?? 0,
      rows: unwrapList<ExplainRow>(body.rows),
    };
  },
};
