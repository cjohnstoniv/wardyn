/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Each person's OWN credential for a model provider (0.8, MP-5) — the typed
// key/token door behind "Add your key" / "Add your token" on Getting started
// and the New Run rail. Mirrors internal/api/model_provider_credentials.go:
// PUT/DELETE /model-providers/{id}/credential, on the authenticated group, not
// operatorOnly — every person, admins included, writes only their own
// namespace, and the server decides who may. Write-only: nothing ever reads a
// stored value back.
import { errText, HttpError, wfetch } from "./core";

export const modelProviderCredentials = {
  // PUT /api/v1/model-providers/{id}/credential  {"value": "..."} -> 204.
  // Stores or overwrites the caller's own key or token for provider `id`.
  // Refused (422) for a sign-in kind (bedrock_sso, anthropic_subscription —
  // use model-provider-signin.ts instead), or a value shorter than the
  // secret-masking floor; the body's `error` names it verbatim.
  async putCredential(id: string, value: string): Promise<void> {
    const res = await wfetch(`/model-providers/${encodeURIComponent(id)}/credential`, {
      method: "PUT",
      body: JSON.stringify({ value }),
    });
    if (!res.ok) {
      throw new HttpError(res.status, await errText(res));
    }
  },

  // DELETE /api/v1/model-providers/{id}/credential -> 204. Idempotent:
  // removing a credential never stored answers 204 too, matching
  // secrets.ts's deleteSecret / ssh-keys.ts's deleteKey.
  async deleteCredential(id: string): Promise<void> {
    const res = await wfetch(`/model-providers/${encodeURIComponent(id)}/credential`, { method: "DELETE" });
    if (!res.ok) {
      throw new HttpError(res.status, await errText(res));
    }
  },
};
