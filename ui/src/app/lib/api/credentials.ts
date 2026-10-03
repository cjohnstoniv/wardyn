/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin view: who holds a credential for each model provider (design F-1/F-2,
// CS-6 PR #1164 — internal/api/credential_inventory.go) and erasing a
// person's whole namespace (CS-5, design F-5/F-6 — internal/api/credential_erase.go).
// Both admin tiers read/act (K5-A); a member is refused at the route.
import { asJson, errText, HttpError, unwrapList, wfetch } from "./core";

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

/** One token an admin created for another person (#1477): metadata only, never a value. */
export interface AdminMintedToken {
  id: string;
  principal: string;
  /** The owner's email; absent when none is known. */
  email?: string;
  /** The token's name. */
  name: string;
  /** The admin who created it. */
  minted_by: string;
  created_at: string;
  last_used_at?: string;
  revoked_at?: string;
  /** When the token stops authenticating; absent when it never expires. */
  expires_at?: string;
}

export interface EraseResult {
  count: number;
  /** Rows under the person's destroyed principal key, unreadable now. */
  crypto_erased?: number;
  /** The rest of `count`: deleted only, gone to the backup horizon. */
  deleted?: number;
  store?: string;
  purged?: boolean;
  recoverable_days?: number;
}

/** The scopes POST /people/{principal}/erasure takes (internal/erasure). */
export type ErasureScope =
  | "credentials"
  | "audit_personal_fields"
  | "run_tasks"
  | "run_outputs"
  | "mask_copies"
  | "recordings";

/** The 200: every scope asked for finished. `outcome` maps each to "done". */
export interface PersonErasureResult {
  person: string;
  scopes: ErasureScope[];
  outcome: Record<string, string>;
}

/** The 500 `erasure_incomplete`: what finished and what is left, so a retry names the same scopes. */
export class ErasureIncomplete extends HttpError {
  done: string[];
  remaining: string[];
  constructor(message: string, done: string[], remaining: string[]) {
    super(500, message, "erasure_incomplete");
    this.done = done;
    this.remaining = remaining;
    this.name = "ErasureIncomplete";
  }
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

  // GET /api/v1/tokens?minted_for_others=true (#1477): the unrevoked tokens an
  // admin created for someone else, metadata only — the response never carries
  // a token value. Both admin tiers read it. The filter is the server's, and is
  // applied again here: a daemon that predates the parameter answers with EVERY
  // token, and the console must never call a person's own token one an admin
  // created for them.
  async listAdminMintedTokens(): Promise<AdminMintedToken[]> {
    const res = await wfetch("/tokens?minted_for_others=true", { method: "GET" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return unwrapList<AdminMintedToken>(await asJson<unknown>(res)).filter(
      (t) => !!t.minted_by && t.minted_by !== t.principal && !t.revoked_at,
    );
  },

  // DELETE /api/v1/tokens/{id}: revoke anyone's token (the existing admin
  // power). 204; any other answer throws, so a token that is still live is
  // never reported revoked.
  async revokeToken(id: string): Promise<void> {
    const res = await wfetch(`/tokens/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
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
  // POST /api/v1/people/{principal}/erasure (security tier): erase a person's
  // records by explicit scope, in one audited act. A partial run is a 500
  // `erasure_incomplete` naming what finished (ErasureIncomplete); any other
  // refusal is an HttpError carrying the server's sentence.
  async erasePerson(principal: string, scopes: ErasureScope[]): Promise<PersonErasureResult> {
    const res = await wfetch(`/people/${encodeURIComponent(principal)}/erasure`, {
      method: "POST",
      body: JSON.stringify({ scopes }),
    });
    if (res.status === 500) {
      const body = (await res.clone().json().catch(() => null)) as {
        error?: string;
        reason?: string;
        done?: unknown;
        remaining?: unknown;
      } | null;
      if (body?.reason === "erasure_incomplete" && Array.isArray(body.remaining)) {
        const names = (v: unknown) => (Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : []);
        throw new ErasureIncomplete(body.error ?? "", names(body.done), names(body.remaining));
      }
    }
    return asJson<PersonErasureResult>(res);
  },
};
