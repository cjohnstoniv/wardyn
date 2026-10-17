/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Key domains (internal/api/key_domains.go): the domains the deployment's key
// service declares, and the assignments that say which one a person's NEXT key
// is made in. Both admin tiers that reach the security routes read and write;
// a deployment without the Postgres store answers 501 on every route.
import { asJson, errText, HttpError, wfetch } from "./core";

/** One domain of GET /key-domains (Go keyDomainRow). */
export interface KeyDomainRow {
  domain: string;
  declared: boolean;
  live_keys: number;
  /** Where the domain's key is; absent when the deployment does not say. */
  key?: string;
  /** True for a domain declared in the deployment's file: boot proved it. */
  proven: boolean;
}

/** One assignment (Go keydomain.Assignment). */
export interface KeyDomainAssignment {
  subject_type: KeyDomainSubjectType;
  subject: string;
  domain: string;
  set_by: string;
  set_at: string;
}

export type KeyDomainSubjectType = "user" | "group" | "all";

/** GET /key-domains (Go keyDomainsResponse). */
export interface KeyDomains {
  domains: KeyDomainRow[];
  assignments: KeyDomainAssignment[];
  /** WARDYN_PRINCIPAL_KEYS is on; off, domains place audit-record keys only. */
  principal_keys: boolean;
}

/** What a write did: applied now, or held for a second person (202). */
export type KeyDomainWrite = "applied" | "pending";

const assignmentPath = (type: KeyDomainSubjectType, subject: string) =>
  `/key-domains/assignments/${type}/${encodeURIComponent(type === "all" ? "all" : subject)}`;

export const keyDomains = {
  async list(): Promise<KeyDomains> {
    const res = await wfetch("/key-domains", { method: "GET" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return asJson<KeyDomains>(res);
  },

  // PUT: 200/201 applied; 202 is a four-eyes hold, and nothing has changed yet.
  async set(type: KeyDomainSubjectType, subject: string, domain: string): Promise<KeyDomainWrite> {
    const res = await wfetch(assignmentPath(type, subject), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ domain }),
    });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return res.status === 202 ? "pending" : "applied";
  },

  // DELETE: 204 applied; 202 is a four-eyes hold.
  async remove(type: KeyDomainSubjectType, subject: string): Promise<KeyDomainWrite> {
    const res = await wfetch(assignmentPath(type, subject), { method: "DELETE" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return res.status === 202 ? "pending" : "applied";
  },
};
