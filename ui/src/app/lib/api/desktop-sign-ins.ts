/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { asJson, wfetch } from "./core";

/** GET /runs/{id}/desktop/sign-ins and the pause and resume answers (mirrors Go's desktopSignInsView).
 *  `store` is "memory" (sign-ins kept in the machine's memory), "none" (the admin set Not allowed, so the
 *  desktop keeps none) or "" (the run's guest predates the store: say nothing). */
export interface DesktopSignInsView {
  store: "memory" | "none" | "";
  paused: boolean;
  paused_at?: string;
  can_pause: boolean;
  /** The run's agent by its display name. */
  agent: string;
}

const base = (runId: string) => `/runs/${encodeURIComponent(runId)}/desktop/sign-ins`;

export const desktopSignIns = {
  async get(runId: string, signal?: AbortSignal): Promise<DesktopSignInsView> {
    return asJson<DesktopSignInsView>(await wfetch(base(runId), { method: "GET", signal }));
  },
  async pause(runId: string): Promise<DesktopSignInsView> {
    return asJson<DesktopSignInsView>(await wfetch(`${base(runId)}/pause`, { method: "POST", asPerson: true }));
  },
  async resume(runId: string): Promise<DesktopSignInsView> {
    return asJson<DesktopSignInsView>(await wfetch(`${base(runId)}/resume`, { method: "POST", asPerson: true }));
  },
};
