/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin People page's reads and writes (internal/api/people.go, sessions.go, sshkeys_admin.go).
// All securityOps: admin or security admin. Stored-credential erase is credentials.erase.
import type { PersonList, PersonToken } from "../types";
import { asJson, errText, HttpError, unwrapList, wfetch } from "./core";

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

  // GET /api/v1/people/{principal}/tokens -> their tokens, revoked ones included.
  async tokens(principal: string): Promise<PersonToken[]> {
    return unwrapList<PersonToken>(await asJson<unknown>(await ok(await wfetch(`/people/${seg(principal)}/tokens`, { method: "GET" }))));
  },

  // POST /api/v1/sessions/revoke {sub} -> 204: the session cutoff, plus their tokens and SSH keys.
  async signOutEverywhere(principal: string): Promise<void> {
    await ok(await wfetch("/sessions/revoke", { method: "POST", body: JSON.stringify({ sub: principal }) }));
  },

  // DELETE /api/v1/people/{principal}/ssh-keys -> {count}.
  async removeSSHKeys(principal: string): Promise<number> {
    const body = await asJson<{ count: number }>(await ok(await wfetch(`/people/${seg(principal)}/ssh-keys`, { method: "DELETE" })));
    return body.count;
  },
};
