/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Platform secrets — NAMES only on read; values are write-only.
import type { SecretName } from "../types";
import { asJson, errEnvelope, errText, HttpError, wfetch } from "./core";

// The namespace an explicit route names: the caller's own (/me/secrets) or the shared administrators' one
// (/shared-secrets). Unlike /secrets, which places a write by the caller's role when it arrives, the server never
// picks it, so a role change between reading a row and acting on it cannot move the act to another namespace.
export type SecretScope = "personal" | "shared";
const scopeBase = (scope?: SecretScope) => (scope === "personal" ? "/me/secrets" : scope === "shared" ? "/shared-secrets" : "/secrets");

export const secrets = {
  // GET /api/v1/secrets -> { names: [...] }. Returns secret NAMES only; values
  // are write-only and never surfaced by the API. Unchanged by 0.7's `mine`
  // addition (migration 0050, per-principal secrets) — its three existing
  // callers keep reading only `names`, whose own semantics are unchanged for
  // them; see listSecretsMine for the additive read.
  async listSecrets(): Promise<SecretName[]> {
    const res = await wfetch("/secrets", { method: "GET" });
    const payload = await asJson<{ names?: unknown }>(res);
    return Array.isArray(payload?.names) ? (payload.names as SecretName[]) : [];
  },

  // GET /api/v1/secrets -> { names: [...], mine: [...] } (0.7, migration
  // 0050): `mine` is always the caller's own namespace's rows — the member
  // Getting Started "Your model key" section's read (own key set, or
  // provided-by-admin). `names` rides along with its listSecrets meaning
  // (see above) for a caller that wants both in one round trip.
  async listSecretsMine(): Promise<{ names: SecretName[]; mine: SecretName[] }> {
    const res = await wfetch("/secrets", { method: "GET" });
    const payload = await asJson<{ names?: unknown; mine?: unknown }>(res);
    return {
      names: Array.isArray(payload?.names) ? (payload.names as SecretName[]) : [],
      mine: Array.isArray(payload?.mine) ? (payload.mine as SecretName[]) : [],
    };
  },

  // GET /api/v1/me/secrets or /shared-secrets -> { names: [...] }: one namespace's names, the route naming which.
  async listScoped(scope: SecretScope): Promise<SecretName[]> {
    const res = await wfetch(scopeBase(scope), { method: "GET" });
    const payload = await asJson<{ names?: unknown }>(res);
    return Array.isArray(payload?.names) ? (payload.names as SecretName[]) : [];
  },

  // PUT /api/v1/secrets/{name}  { value } -> 204. Stores or overwrites a named
  // secret; the value is write-only. `scope` sends it to that namespace's route.
  // `createOnly` sends If-None-Match: *, so a name the namespace already holds is
  // refused (409, lib/secret-refusals.ts) instead of replaced: a list read earlier
  // can be stale, and only the server knows what is stored now.
  async setSecret(name: string, value: string, opts?: { scope?: SecretScope; createOnly?: boolean }): Promise<void> {
    const res = await wfetch(`${scopeBase(opts?.scope)}/${encodeURIComponent(name)}`, {
      method: "PUT", asPerson: true,
      headers: opts?.createOnly ? { "If-None-Match": "*" } : undefined,
      body: JSON.stringify({ value }),
    });
    if (!res.ok) {
      const env = await errEnvelope(res);
      throw new HttpError(res.status, env.message, env.reason);
    }
  },

  // DELETE /api/v1/secrets/{name} -> 204. The name is resolved in the namespace
  // of whoever the session names when the request arrives, so it names a person:
  // `person` is the one whose row was confirmed (getExpectedPrincipal, read when
  // Delete was chosen), and the server refuses the delete (412, nothing removed)
  // if someone else has signed in since. Without it, or with "" (Delete chosen
  // before this tab knew who was signed in), the person this tab has confirmed
  // by the time it is sent, as setSecret does. `scope` sends it to the route of
  // the namespace the row was read from.
  async deleteSecret(name: string, person?: string, scope?: SecretScope): Promise<void> {
    const res = await wfetch(`${scopeBase(scope)}/${encodeURIComponent(name)}`, { method: "DELETE", asPerson: person || true });
    if (!res.ok && res.status !== 404) {
      throw new HttpError(res.status, await errText(res));
    }
  },
};
