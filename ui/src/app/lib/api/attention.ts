/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GET /api/v1/me/attention (#1197 L1b) — the shell's two nav-badge counts in
// one small object, replacing App.tsx's old refreshBadges (two unscoped
// LIST_LIMIT reads of /runs and /approvals, joined client-side via
// board-groups.ts's approvalSignals/needsAttention). See
// internal/api/run_attention.go's handleMeAttention for the exact scope each
// count carries.
import { asJson, wfetch } from "./core";

export interface MeAttention {
  /** Live runs in this view's own default scope whose attention.by=="you". */
  needs_you: number;
  /** The scoped PENDING approval count — same scope GET /approvals?state=PENDING gives today. */
  pending_approvals: number;
}

export const attention = {
  // GET /api/v1/me/attention?view=user|admin
  async getMeAttention(view: "user" | "admin"): Promise<MeAttention> {
    const res = await wfetch(`/me/attention?view=${view}`, { method: "GET" });
    return asJson<MeAttention>(res);
  },
};
