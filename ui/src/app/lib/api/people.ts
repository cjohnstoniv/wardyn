/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin People page's reads and writes (internal/api/people.go, sessions.go, sshkeys_admin.go).
// All securityOps: admin or security admin. Stored-credential erase is credentials.erase.
import type { PersonList, PersonSummary, PersonToken } from "../types";
import { asJson, errText, HttpError, LIST_LIMIT, unwrapList, wfetch } from "./core";

const seg = (principal: string) => encodeURIComponent(principal);

async function ok(res: Response): Promise<Response> {
  if (!res.ok) throw new HttpError(res.status, await errText(res));
  return res;
}

export const people = {
  // GET /api/v1/people?limit&cursor&q&state -> one page.
  async list(opts: { limit?: number; cursor?: string; q?: string; state?: "active" | "deactivated" } = {}): Promise<PersonList> {
    const p = new URLSearchParams();
    if (opts.limit) p.set("limit", String(opts.limit));
    if (opts.cursor) p.set("cursor", opts.cursor);
    if (opts.q) p.set("q", opts.q);
    if (opts.state) p.set("state", opts.state);
    const qs = p.toString();
    const body = await asJson<PersonList>(await ok(await wfetch(`/people${qs ? `?${qs}` : ""}`, { method: "GET" })));
    return { people: body.people ?? [], next_cursor: body.next_cursor };
  },

  // POST /api/v1/people {principal, email?} -> 201 (created) or 200 (already there).
  async create(principal: string, email: string): Promise<void> {
    await ok(await wfetch("/people", { method: "POST", body: JSON.stringify({ principal, ...(email ? { email } : {}) }) }));
  },

  // GET /api/v1/people -> the one person with this principal, or undefined. There is no read-one route,
  // so ask for the principal as the search and follow the cursor until the exact principal turns up
  // (the search also matches email prefixes, and those can sort ahead of it).
  async get(principal: string): Promise<PersonSummary | undefined> {
    let cursor: string | undefined;
    do {
      const page = await people.list({ limit: 200, cursor, q: principal });
      const hit = page.people.find((p) => p.principal === principal);
      if (hit) return hit;
      cursor = page.next_cursor;
    } while (cursor);
    return undefined;
  },

  // GET /api/v1/people/{principal}/tokens -> all their tokens, revoked and expired ones included. The
  // route is paged newest first, so this reads every page: an older live token can sit behind a full
  // page of retired ones.
  async tokens(principal: string): Promise<PersonToken[]> {
    const all: PersonToken[] = [];
    for (;;) {
      const res = await ok(await wfetch(`/people/${seg(principal)}/tokens?limit=${LIST_LIMIT}&offset=${all.length}`, { method: "GET" }));
      const page = unwrapList<PersonToken>(await asJson<unknown>(res));
      all.push(...page);
      if (page.length === 0 || res.headers.get("X-Wardyn-Truncated") !== "true") return all;
    }
  },

  // POST /api/v1/sessions/revoke {sub, sessions_only} -> 204: the session cutoff alone. Without
  // sessions_only the route also revokes their API tokens and deletes their SSH keys.
  async signOutEverywhere(principal: string): Promise<void> {
    await ok(await wfetch("/sessions/revoke", { method: "POST", body: JSON.stringify({ sub: principal, sessions_only: true }) }));
  },

  // DELETE /api/v1/people/{principal}/ssh-keys -> {count}.
  async removeSSHKeys(principal: string): Promise<number> {
    const body = await asJson<{ count: number }>(await ok(await wfetch(`/people/${seg(principal)}/ssh-keys`, { method: "DELETE" })));
    return body.count;
  },
};
