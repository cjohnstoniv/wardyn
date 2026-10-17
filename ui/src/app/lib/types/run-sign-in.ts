/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GET /runs/{id}/sign-in — mirrors runSignInResponse (internal/api/run_sign_in.go).
export interface RunSignIn {
  /** "waiting" (a device code is on the sign-in pane) or "not_waiting". */
  state: string;
  /** Only when waiting. */
  verification_url?: string;
  /** Only when waiting. */
  user_code?: string;
}
