/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Finding 7a (0.7.5 field report): the verification tab never opened. The old
// code called `window.open(url, "_blank", "noopener,noreferrer")` from inside
// `onOutput` — a PTY callback, never a user gesture — and every modern browser
// blocks a popup that is not tied to one.
//
// The fix: open the tab ON THE CLICK (the only gesture the flow ever gets),
// keep the handle, and NAVIGATE it once the verification URL appears.
// `window.open(url, "_blank", "noopener")` returns null by spec — there is no
// handle to navigate later — so the pattern here is the standard
// pre-`noopener` mitigation: open blank, then sever `opener` by hand.
// `opener` is a `[Replaceable]` settable attribute, so assigning null works
// and keeps the forward handle (`w.location`, `w.close()`) usable even once
// the document has navigated cross-origin.
export type AuthTab = {
  navigate(url: string): void;
  close(): void;
};

// DRAFT (M2 canon pending) — written into the about:blank tab the moment the
// person CLICKS Start, because that click is the only user gesture the flow
// ever gets. Plain text, no stylesheet, no font, no network — CONSOLE-RULES'
// "no inline hex or ad-hoc sizing" applies to Wardyn's own screens, not to a
// page that exists for a few seconds before AWS's own page replaces it, but a
// plain document is also the simplest thing that cannot fail to render.
// Provider-neutral (O-4: the anthropic flow gets this tab too, by
// construction — both Start buttons call the same `launch()`).
export const AUTH_TAB_PLACEHOLDER_HTML =
  "<!doctype html><meta charset=utf-8><title>Wardyn — waiting for the sign-in page</title>" +
  "<body>" +
  "<p>Wardyn is starting your sign-in sandbox.</p>" +
  "<p>This page changes to your provider's sign-in page by itself — usually within seconds, " +
  "up to a couple of minutes the first time. The code to enter is shown on the Wardyn tab; if nothing happens, " +
  "go back there — the link is on the sign-in panel too.</p>";

// DRAFT (M2 canon pending) — shown ONLY once a verification URL exists and the
// tab never opened even on the click (Safari strict / a managed popup
// policy): 0.7.5's header link is the one-click fallback either way.
export const AUTH_TAB_BLOCKED_NOTE =
  "Your browser blocked the automatic tab — use the link above to open the verification page.";

// openAuthTab opens the placeholder tab SYNCHRONOUSLY (no `await` may precede
// this call — see the loud comment at its one call site) and returns a handle
// to navigate once the real URL is known, or null when the browser blocked
// even a click-backed popup. Every later call on a null handle is a no-op —
// callers hold the return value in a ref and use optional chaining.
export function openAuthTab(): AuthTab | null {
  const w = window.open("", "_blank");
  if (!w) return null;
  // The standard pre-noopener mitigation: sever the reverse link by hand
  // (window.open's own "noopener" feature would return null instead, losing
  // the handle this lane exists to keep).
  w.opener = null;
  // AUTH_TAB_PLACEHOLDER_HTML is a fixed module constant — never
  // user/sandbox-supplied input — so writing it is not an injection surface;
  // document.write is used only because it is the one API that can paint a
  // window opened with an empty URL before any navigation.
  try {
    w.document.write(AUTH_TAB_PLACEHOLDER_HTML);
    w.document.close();
  } catch {
    // A CSP `sandbox` directive (or an ancient Safari) can refuse
    // document.write on an opened window; fall back to a plain text node so
    // the tab is not silently blank while it waits to be navigated.
    try {
      w.document.body.textContent = "Wardyn is starting your sign-in sandbox.";
    } catch {
      /* the tab is still ours to navigate once the URL is known */
    }
  }
  return {
    navigate(url: string) {
      try {
        w.location.href = url;
      } catch {
        // The person may have closed the tab by hand — nothing to navigate.
      }
    },
    close() {
      try {
        if (!w.closed) w.close();
      } catch {
        /* already gone */
      }
    },
  };
}
