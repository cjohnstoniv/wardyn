/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The sign-in doors keyed by model provider (0.8, MP-13) — behind "Sign in to
// AWS" / "Sign in to Claude" wherever a bedrock_sso or anthropic_subscription
// provider's row appears. Mirrors internal/api/provider_signin.go:
// POST/PUT /model-providers/{id}/sign-in, on the authenticated group, not
// operatorOnly — every person, admins included, signs in for themselves, into
// their own namespace under the provider's UID. These answer only while the
// install has a model-provider block; harness-auth.ts's legacy
// /setup/harness-login door answers only while it does not — the two never
// both apply.
import { asJson, errText, HttpError, wfetch } from "./core";

export interface ProviderSignInStarted {
  runId: string;
  // The run's state AS ANSWERED — PENDING, because the answer now precedes
  // dispatch (harness-auth.ts's harnessLogin has the same shape and the same
  // reason: poll runs.getRun to RUNNING before attaching, never read here).
  state: string;
}

export const modelProviderSignIn = {
  // POST /api/v1/model-providers/{id}/sign-in — launch the caller's own
  // sign-in sandbox for provider `id` (AWS device sign-in for bedrock_sso,
  // the Claude container login for anthropic_subscription). No body:
  // everything the sandbox is seeded with — an AWS start URL, region and
  // account/role pin — is admin-owned, read from the provider record itself.
  async startSignIn(id: string): Promise<ProviderSignInStarted> {
    const res = await wfetch(`/model-providers/${encodeURIComponent(id)}/sign-in`, { method: "POST" });
    const body = await asJson<{ run_id: string; state: string }>(res);
    return { runId: body.run_id, state: body.state };
  },

  // PUT /api/v1/model-providers/{id}/sign-in  {"run_id", "token"} -> 204.
  // Stores the caller's own Claude sign-in — the `claude setup-token` output
  // their sign-in sandbox printed — bound to the run startSignIn returned for
  // this same provider. An AWS sign-in is never pasted here: the sandbox's
  // own helper upload stores it directly (this door 422s naming that). 204
  // has no body — do not asJson/res.json() it (matches secrets.ts's
  // setSecret).
  async captureSignIn(id: string, runId: string, token: string): Promise<void> {
    const res = await wfetch(`/model-providers/${encodeURIComponent(id)}/sign-in`, {
      method: "PUT",
      body: JSON.stringify({ run_id: runId, token }),
    });
    if (!res.ok) {
      throw new HttpError(res.status, await errText(res));
    }
  },
};
