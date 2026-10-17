/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The expiry banner's "Sign in again", in place: the click opens the sign-in
// window and starts a renewal (lib/reauth.ts); the lazy reauth layer only
// waits on it. On the eager graph beside the banner, so free of copy and UI.
import { appURL } from "./base-path";
import { useReauth, type Renewal } from "./reauth";

/** Where the reauth layer draws the renewal strip: the banner's own place. */
export const RENEW_STRIP_SLOT = "session-renew-strip";

/** The sign-in window, or null when the browser refused it. Call it inside
 *  the click that asks for it — a browser allows window.open only there.
 *  about:blank first, the opener severed, then navigated. */
export function openSignInWindow(): Window | null {
  const popup = window.open("about:blank", "wardyn-reauth", "width=520,height=680");
  if (!popup) return null;
  popup.opener = null;
  popup.location.href = appURL("/auth/login");
  return popup;
}

/** The banner button's click handler. `from` is who is signed in right now:
 *  the renewal records it, and only that person with that authority and a
 *  later expiry counts as renewed. */
export function useSessionRenew(from: Omit<Renewal, "popup">): () => void {
  const { startRenew } = useReauth();
  return () => startRenew({ ...from, popup: openSignInWindow() });
}
