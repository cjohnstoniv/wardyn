/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The egress baseline routes behind Governance -> Baseline hosts. Both are securityOps; the PUT is held for a
// second person on a deployment that requires one (202, thrown as PendingChangeError by asJson) and takes the
// GET's ETag as If-Match, so a stale draft is a 412, not an overwrite. A PUT replaces both lists.
import { asJson, wfetch } from "./core";

/** The declaration as the server stores it: sorted, never null. */
export interface EgressBaseline {
  baseline_hosts: string[];
  internal_host_suffixes: string[];
}

/** The declaration with the ETag the response carried, which the next PUT sends back as If-Match. */
export type EgressBaselineSnapshot = EgressBaseline & { etag?: string | null };

export const egressBaseline = {
  // GET /api/v1/governance/egress-baseline.
  async get(): Promise<EgressBaselineSnapshot> {
    const res = await wfetch("/governance/egress-baseline", { method: "GET" });
    return { ...(await asJson<EgressBaseline>(res)), etag: res.headers.get("ETag") };
  },

  // PUT /api/v1/governance/egress-baseline: 200 -> the stored declaration, 202 -> PendingChangeError,
  // 400 -> HttpError carrying the server's sentence, 412 -> the same, for a stale `etag`.
  async put(body: EgressBaseline, etag?: string | null): Promise<EgressBaselineSnapshot> {
    const res = await wfetch("/governance/egress-baseline", {
      method: "PUT", asPerson: true,
      headers: etag ? { "If-Match": etag } : {},
      body: JSON.stringify(body),
    });
    return { ...(await asJson<EgressBaseline>(res)), etag: res.headers.get("ETag") };
  },
};
