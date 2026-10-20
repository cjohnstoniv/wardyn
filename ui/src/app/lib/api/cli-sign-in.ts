/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// CLI sign-in, the signed-in person's half: the approve page's three calls and Account's Signed-in CLIs.
// The anonymous half (create, token, refresh) belongs to the CLI and is not here.
import type {
  CLISession,
  CLISignInApproveResponse,
  CLISignInDenyResponse,
  CLISignInView,
} from "../types/cli-sign-in";
import { asJson, asNoContent, unwrapList, wfetch, withLimit } from "./core";

const signInPath = (id: string) => `/me/cli-sign-ins/${encodeURIComponent(id)}`;

export const cliSignIn = {
  // GET /api/v1/me/cli-sign-ins/{id}; 404 cli_sign_in_not_found, 410 cli_sign_in_used / cli_sign_in_expired.
  async get(id: string): Promise<CLISignInView> {
    return asJson<CLISignInView>(await wfetch(signInPath(id), { method: "GET" }));
  },

  // POST .../approve runs mintGuards, so it is refused to a token, in the user view, and without the api_token feature.
  async approve(id: string): Promise<CLISignInApproveResponse> {
    return asJson<CLISignInApproveResponse>(await wfetch(`${signInPath(id)}/approve`, { method: "POST", asPerson: true }));
  },

  async deny(id: string): Promise<CLISignInDenyResponse> {
    return asJson<CLISignInDenyResponse>(await wfetch(`${signInPath(id)}/deny`, { method: "POST", asPerson: true }));
  },

  // GET /api/v1/me/cli-sessions -> the caller's live CLI sessions.
  async listSessions(): Promise<CLISession[]> {
    return unwrapList<CLISession>(await asJson<unknown>(await wfetch(withLimit("/me/cli-sessions"), { method: "GET" })));
  },

  // DELETE /api/v1/me/cli-sessions/{id} -> 204 (reason cli_session_not_found when it already ended).
  async signOut(id: string): Promise<void> {
    await asNoContent(await wfetch(`/me/cli-sessions/${encodeURIComponent(id)}`, { method: "DELETE" }));
  },
};
