/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// User types CRUD (0.8, UT-1) — the four securityOps routes behind the User
// types screen. Mirrors internal/api/user_types.go; every route is under
// /api/v1 via wfetch.
import type { UserType, UserTypeInput } from "../types";
import { asJson, errText, HttpError, unwrapList, wfetch } from "./core";

export const userTypes = {
  // GET /api/v1/user-types -> {user_types}. A nil Go slice encodes as null,
  // so this is coerced and every caller can map over it directly.
  async listUserTypes(): Promise<UserType[]> {
    const res = await wfetch("/user-types", { method: "GET" });
    const body = await asJson<{ user_types?: UserType[] | null }>(res);
    return unwrapList<UserType>(body.user_types);
  },

  // POST /api/v1/user-types -> 201 the saved row. A 409 names the id or name
  // that collided (handleCreateUserType).
  async createUserType(input: UserTypeInput): Promise<UserType> {
    const res = await wfetch("/user-types", { method: "POST", body: JSON.stringify(input) });
    return asJson<UserType>(res);
  },

  // PUT /api/v1/user-types/{id} -> 200 the saved row. id in the body, when
  // present, must equal the path's — the id never changes.
  async updateUserType(id: string, input: UserTypeInput): Promise<UserType> {
    const res = await wfetch(`/user-types/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: JSON.stringify(input),
    });
    return asJson<UserType>(res);
  },

  // DELETE /api/v1/user-types/{id} -> 204. The five-source delete guard
  // (role_mappings, capability_grants, governance_assignments,
  // user_drive_grants, agent_runs) answers 409 with the count; that message
  // reaches the caller verbatim. A 404 is tolerated as "already gone".
  async deleteUserType(id: string): Promise<void> {
    const res = await wfetch(`/user-types/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) {
      throw new HttpError(res.status, await errText(res));
    }
  },
};
