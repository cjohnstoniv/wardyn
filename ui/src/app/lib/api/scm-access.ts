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
  // GET /api/v1/me/scm-access. A deployment with no Azure DevOps row answers
  // {} (state ""), never a 404 — the absent-row doctrine.
  async getMine(): Promise<SCMAccess> {
    const res = await wfetch("/me/scm-access", { method: "GET" });
    return asJson<SCMAccess>(res);
  },
};
