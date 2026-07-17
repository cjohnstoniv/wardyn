/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Wardyn-managed harness auth (subscription/setup-token capture): launch an
// interactive login sandbox, store a pasted setup-token, or disconnect.
import { asJson, wfetch } from "./core";

export const harnessAuth = {
  // POST /api/v1/setup/harness-login — launch an interactive login sandbox for a
  // provider (default "anthropic"); returns the run id to attach to.
  async harnessLogin(provider = "anthropic"): Promise<string> {
    const res = await wfetch("/setup/harness-login", {
      method: "POST",
      body: JSON.stringify({ provider }),
    });
    const body = await asJson<{ run_id: string }>(res);
    return body.run_id;
  },

  // PUT /api/v1/setup/harness-credential/{provider} — store the operator-pasted
  // `claude setup-token` output. Write-only; the value is never returned.
  async harnessCredentialPaste(provider: string, token: string): Promise<void> {
    const res = await wfetch(`/setup/harness-credential/${encodeURIComponent(provider)}`, {
      method: "PUT",
      body: JSON.stringify({ token }),
    });
    await asJson<unknown>(res);
  },

  // DELETE /api/v1/setup/harness-credential/{provider} — disconnect (delete the
  // stored managed credential).
  async harnessDisconnect(provider: string): Promise<void> {
    const res = await wfetch(`/setup/harness-credential/${encodeURIComponent(provider)}`, {
      method: "DELETE",
    });
    await asJson<unknown>(res);
  },
};
