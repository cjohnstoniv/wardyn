/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The ONE place a client URL is built. WARDYN_BASE_PATH (docs/ENV.md) serves
// the console and API under a sub-path behind a reverse proxy; the daemon
// writes it into the served index.html as <html data-wardyn-base="/wardyn">
// (internal/api's serveIndex) — an attribute, because the CSP allows neither
// an inline script nor a <base> element. "" is the host root, which is also
// what the vite dev server and the test DOM carry. base-path.guard.test.ts
// refuses a root-absolute server URL anywhere else in src/.

export function basePath(): string {
  return typeof document === "undefined" ? "" : (document.documentElement.dataset.wardynBase ?? "");
}

/** A root-absolute server path or console route ("/healthz", "/runs/…"), under the base path. */
export function appURL(path: string): string {
  return basePath() + path;
}

/** An API path ("/runs"), under /api/v1 and the base path. */
export function apiURL(path: string): string {
  return appURL(`/api/v1${path}`);
}

/** The ws:// or wss:// URL of an API path on this origin (same-origin, so the session cookie rides along). */
export function wsURL(path: string): string {
  return originWS(apiURL(path));
}

/** The ws:// or wss:// URL of a path that already carries its base, on this origin (the desktop viewer's
 *  stream, whose path the UI gateway writes with its own base path). */
export function originWS(path: string): string {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}${path}`;
}

/** A browser pathname as the router sees it: the base path taken off. */
export function routerPath(pathname: string = window.location.pathname): string {
  const base = basePath();
  if (!base || (pathname !== base && !pathname.startsWith(`${base}/`))) return pathname;
  return pathname.slice(base.length) || "/";
}

/** The SSO sign-in link. Off the console root it carries the page the person is on (return_to), so the
 *  server brings them back to it; the server accepts only a console path and falls back to the root. */
export function signInURL(): string {
  const here = routerPath() + window.location.search;
  return appURL("/auth/login") + (routerPath() === "/" ? "" : `?return_to=${encodeURIComponent(here)}`);
}
