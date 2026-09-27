/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { ReauthContext } from "./reauth";

/** True while the console is signed out mid-page — the "Sign in to continue"
 *  dialog or the read-only bar (#483). A live channel that does not go through
 *  wfetch (the terminal's WebSocket) closes and stays closed while this holds:
 *  whoever holds the session by then may not be the person at the keyboard. */
export function useSignedOut(): boolean {
  return React.useContext(ReauthContext).phase !== "none";
}
