/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Finding 7a (0.7.5 field report): a tab opened from inside a PTY callback
// (`onOutput`), never a user gesture, is blocked by every modern browser —
// the tab must open on the click (the only gesture this flow gets), keep the
// handle, and navigate it once the verification URL appears.
// `window.open(url, "_blank", "noopener")` returns null by spec — there is no
// handle to navigate later — so the pattern here is the standard
// pre-`noopener` mitigation: open blank, then sever `opener` by hand.
// `opener` is a `[Replaceable]` settable attribute, so assigning null works
// and keeps the forward handle (`w.location`, `w.close()`) usable even once
// the document has navigated cross-origin.
//
// review-1 B2, coordinator ruling (option A) — `close()` is best-effort, not
// guaranteed, once the tab has navigated. Chromium (verified in e2e) refuses
// `.close()` — and any further `.location` write — on a window it did not
// itself open with script, once that window's `opener` has been disowned and
// it has navigated cross-origin: the tab-nabbing mitigation above is exactly
// what makes the browser stop trusting this handle as "ours" past that point.
// The alternative — not severing `opener` — was rejected: it would hand the
// provider's own page (a *.awsapps.com / claude.ai origin) a live reference
// back into the console tab via `window.opener`, which is the leak Finding
// 7a exists to close, not a bug to trade away for a `close()` that always
// works. So: before navigation, `close()` reliably closes the placeholder
// (every exit path in the pane still does this). After navigation, the
// provider's own page — the one the person approves — is the tab's end
// state; the pane's `closeAuthTab()` calls are harmless no-ops past that
// point (caught, never thrown) rather than something to route around.
export type AuthTab = {
  navigate(url: string): void;
  close(): void;
};

// DRAFT (M2 canon pending) — written into the about:blank tab the moment the
// person clicks Start, because that click is the only user gesture the flow
// ever gets. Plain text, no stylesheet, no font, no network — CONSOLE-RULES'
// "no inline hex or ad-hoc sizing" applies to Wardyn's own screens, not to a
// page that exists for a few seconds before AWS's own page replaces it, but a
// plain document is also the simplest thing that cannot fail to render.
// Provider-neutral: the anthropic flow gets this tab too, by construction —
// both Start buttons call the same `launch()`.
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
