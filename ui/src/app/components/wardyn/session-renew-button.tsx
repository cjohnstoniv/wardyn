/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The session-expiry banner's action (app-shell.tsx draws the banner): a
// button that signs in again in place instead of navigating the tab away.
import { RENEW_STRIP_SLOT, useSessionRenew } from "../../lib/use-session-renew";
import { SIGN_IN_AGAIN } from "../../lib/session-renew-copy";
import type { ShellMeta } from "../screens/app-shell";

export function SessionRenewButton({ meta }: { meta: ShellMeta }) {
  const renew = useSessionRenew({
    principal: meta.principal,
    role: meta.role,
    operator: meta.operator,
    securityOperator: meta.securityOperator,
    expiresAt: meta.sessionExpiresAt?.getTime() ?? 0,
  });
  return (
    <button type="button" onClick={renew} className="font-medium underline underline-offset-2">
      {SIGN_IN_AGAIN}
    </button>
  );
}

/** Where the lazy reauth layer draws the renewal strip while a renewal waits:
 *  the banner's own place in the stack. Empty the rest of the time. */
export function SessionRenewSlot() {
  return <div id={RENEW_STRIP_SLOT} />;
}
