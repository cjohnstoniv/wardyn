/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Platform secrets — NAMES only on read; values are write-only.
import type { SecretName } from "../types";
import { asJson, errText, HttpError, wfetch } from "./core";

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

  // PUT /api/v1/secrets/{name}  { value } -> 204. Stores or overwrites a named
  // secret; the value is write-only.
  async setSecret(name: string, value: string): Promise<void> {
    const res = await wfetch(`/secrets/${encodeURIComponent(name)}`, {
      method: "PUT",
      body: JSON.stringify({ value }),
    });
    if (!res.ok) {
      throw new HttpError(res.status, await errText(res));
    }
  },

  // DELETE /api/v1/secrets/{name} -> 204.
  async deleteSecret(name: string): Promise<void> {
    const res = await wfetch(`/secrets/${encodeURIComponent(name)}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) {
      throw new HttpError(res.status, await errText(res));
    }
  },
};
