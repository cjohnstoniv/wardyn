/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// This caller's own Azure DevOps access state (internal/api.SCMAccess, #386)
// — the same self-service shape ssh-keys.ts is: scoped server-side to the
// signed-in principal, no admin view of anyone else's.
import type { SCMAccess } from "../types";
import { asJson, wfetch } from "./core";

export const scmAccess = {
  // GET /api/v1/me/scm-access. One entry per per-user Azure DevOps row
  // (review finding F6) — [] when there is none (no row at all, or the
  // deployment's only row is shared), never a 404. Today's server ever
  // returns 0 or 1 entries (scmaccess.go's file doc), but every caller
  // reads it as a list.
  async getMine(): Promise<SCMAccess[]> {
    const res = await wfetch("/me/scm-access", { method: "GET" });
    return asJson<SCMAccess[]>(res);
  },
};
