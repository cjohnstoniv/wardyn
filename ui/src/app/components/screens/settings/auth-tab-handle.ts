/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Finding 7a (0.7.5 field report): a tab opened outside a user gesture — from
// a PTY callback or a poll tick — is blocked by every modern browser. Until
// #628 the click that STARTED the sign-in opened a placeholder about:blank tab
// and navigated it later, which left the person on a bare page for the whole
// of a cold image pull. Now the dialog keeps the progress, and the tab opens
// only from the "Open … sign-in" button's own click, once the provider's page
// exists — a second, fresh gesture.
//
// `window.open(url, "_blank", "noopener")` returns null by spec, which would
// make a blocked popup indistinguishable from an opened one, so the pattern is
// the standard pre-`noopener` mitigation: open blank, sever `opener` by hand,
// then navigate. Severing is the point: the provider's own page (a
// *.awsapps.com / claude.ai origin) must never hold a live `window.opener`
// reference back into the console tab.
//
// openSignInTab must be called synchronously inside the click handler (no
// `await` before it). False means the browser blocked even that; the dialog's
// copy-link fallback is on screen either way.
export function openSignInTab(url: string): boolean {
  const w = window.open("", "_blank");
  if (!w) return false;
  w.opener = null;
  try {
    w.location.href = url;
  } catch {
    // Closed by hand between the two lines — nothing to navigate.
  }
  return true;
}
