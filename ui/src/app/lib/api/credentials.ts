/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin view: who holds a credential for each model provider (design F-1/F-2,
// CS-6 PR #1164 — internal/api/credential_inventory.go) and erasing a
// person's whole namespace (CS-5, design F-5/F-6 — internal/api/credential_erase.go).
// Both admin tiers read/act (K5-A); a member is refused at the route.
import { asJson, errText, HttpError, wfetch } from "./core";

export interface CredentialRow {
  person: string;
  /** The paired email (design F-2); absent when none is known. */
  email?: string;
  provider: string;
  /** The provider's display name (design F-2); absent for a provider the
   *  server no longer has a name for. */
  provider_name?: string;
  state: "stored" | "expired";
  store: "pg" | "vaultkv" | "azurekv";
  added_at: string;
  last_used_at?: string;
  expires_at?: string;
}

export interface CredentialInventory {
  credentials: CredentialRow[];
  counts: { people: number; credentials: number; by_provider: Record<string, number> };
}

export interface EraseResult {
  count: number;
  store?: string;
  purged?: boolean;
  recoverable_days?: number;
}

export const credentials = {
  // GET /api/v1/model-providers/credentials -> the inventory. 503
  // (credInventoryNoMeta) when the configured secret store keeps no
  // credential metadata at all — the body's `error` names it verbatim.
  async listInventory(): Promise<CredentialInventory> {
    const res = await wfetch("/model-providers/credentials", { method: "GET" });
    if (!res.ok) {
      throw new HttpError(res.status, await errText(res));
    }
    return asJson<CredentialInventory>(res);
  },

  // DELETE /api/v1/people/{principal}/credentials -> the erase result.
  // `principal` resolves exactly as the secrets routes' ?owner= does — an
  // email, an OIDC subject, or the bare subject an inventory row names.
  async erase(principal: string): Promise<EraseResult> {
    const res = await wfetch(`/people/${encodeURIComponent(principal)}/credentials`, { method: "DELETE" });
    if (!res.ok) {
      throw new HttpError(res.status, await errText(res));
    }
    return asJson<EraseResult>(res);
  },
};
