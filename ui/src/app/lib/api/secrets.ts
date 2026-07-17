/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Platform secrets — NAMES only on read; values are write-only.
import type { SecretName } from "../types";
import { asJson, errText, HttpError, wfetch } from "./core";

export const secrets = {
  // GET /api/v1/secrets -> { names: [...] }. Returns secret NAMES only; values
  // are write-only and never surfaced by the API.
  async listSecrets(): Promise<SecretName[]> {
    const res = await wfetch("/secrets", { method: "GET" });
    const payload = await asJson<{ names?: unknown }>(res);
    return Array.isArray(payload?.names) ? (payload.names as SecretName[]) : [];
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
