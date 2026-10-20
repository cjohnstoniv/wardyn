/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// CLI sign-in (wardyn login), the console half. Each interface has exactly the json tags of the Go struct
// it mirrors in internal/types/cli_sign_in.go (lane ATT-LOGIN-SRV, which lands in the same train).

/** cli_sign_ins.mode: loopback moves the browser to the CLI's callback; paste shows a code to carry back. */
export type CLISignInMode = "loopback" | "paste";

/** The CLI's credential store, as the CLI reports it. */
export type CLIStore = "keychain" | "file";

/** internal/types CLISignInView: GET /me/cli-sign-ins/{id}, what the approve page shows. */
export interface CLISignInView {
  id: string;
  machine: string;
  user_code: string;
  mode: CLISignInMode;
  store: CLIStore;
  /** wardyn login --admin: the page asks for a 1-hour admin step-up, not a session. */
  admin: boolean;
  request_address: string;
  approver_address: string;
  /** Advisory: behind an unlisted proxy both sides read the proxy and always match. */
  address_match: boolean;
  created_at: string;
  expires_at: string;
  session_ttl_seconds: number;
  session_idle_seconds: number;
  access_ttl_seconds: number;
}

/** internal/types CLISignInApproveResponse: POST /me/cli-sign-ins/{id}/approve. */
export interface CLISignInApproveResponse {
  /** K, single use: shown to paste in paste mode, already inside redirect_url in loopback mode. */
  code: string;
  code_expires_in: number;
  redirect_url?: string;
}

/** internal/types CLISignInDenyResponse: POST /me/cli-sign-ins/{id}/deny. */
export interface CLISignInDenyResponse {
  redirect_url?: string;
}

/** internal/types CLISession: one row of GET /me/cli-sessions ("Signed-in CLIs"); live sessions only. */
export interface CLISession {
  id: string;
  machine: string;
  store: CLIStore;
  created_at: string;
  last_refresh_at: string;
  expires_at: string;
  idle_expires_at: string;
  /** Set while a cli_admin step-up on that computer is live. */
  admin_expires_at?: string;
}
