/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { asJson, wfetch } from "./core";

export type SupportDoor = "terminal" | "commands" | "files" | "forward" | "web_apps" | "desktop";

/** A help request or session as the support routes answer it (Go: supportGrantView). `reason` is the helper's
 *  own words: the owner reads it, and the helper while the grant is open; never once it has ended. */
export interface SupportGrantView {
  grant: string;
  helper: string;
  reason?: string;
  doors_asked: SupportDoor[];
  doors?: SupportDoor[];
  minutes?: number;
  requested_at: string;
  expires_at?: string;
  last_seen_at?: string;
  ended_at?: string;
  /** requested (waiting for the owner), active (accepted, not ended) or ended. */
  state: "requested" | "active" | "ended";
  end_reason?: string;
  /** On a grant that is not ended: the doors Wardyn records on this run (terminal, desktop). */
  recorded_doors?: SupportDoor[];
}

/** GET /runs/{id}/support. The helper of a grant that ended reads `ended` and `doors_offered` only. */
export interface SupportStatus {
  open?: SupportGrantView | null;
  ended?: SupportGrantView | null;
  doors_offered?: SupportDoor[];
  recorded_doors?: SupportDoor[];
}

const base = (runId: string) => `/runs/${encodeURIComponent(runId)}/support`;
const one = (runId: string, grant: string, verb: string) => `${base(runId)}/${encodeURIComponent(grant)}/${verb}`;
const post = (body?: unknown) => ({ method: "POST", asPerson: true as const, ...(body === undefined ? {} : { body: JSON.stringify(body) }) });

export const support = {
  async status(runId: string, signal?: AbortSignal): Promise<SupportStatus> {
    return asJson<SupportStatus>(await wfetch(base(runId), { method: "GET", signal }));
  },
  async request(runId: string, doors: SupportDoor[], reason: string): Promise<SupportGrantView> {
    return asJson<SupportGrantView>(await wfetch(base(runId), post({ doors, reason })));
  },
  async accept(runId: string, grant: string, doors: SupportDoor[], minutes: number): Promise<SupportGrantView> {
    return asJson<SupportGrantView>(await wfetch(one(runId, grant, "accept"), post({ doors, minutes })));
  },
  async decline(runId: string, grant: string): Promise<SupportGrantView> {
    return asJson<SupportGrantView>(await wfetch(one(runId, grant, "decline"), post()));
  },
  async end(runId: string, grant: string): Promise<SupportGrantView> {
    return asJson<SupportGrantView>(await wfetch(one(runId, grant, "end"), post()));
  },
};
