/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ONE navigation policy for every link the terminal can offer: URLs xterm
// detects in the text (WebLinksAddon) and OSC 8 hyperlinks (xterm's
// `linkHandler`). Both go through decideLink, so they cannot drift into two
// policies. Everything a link says comes from the sandbox, so the rules are:
//
//   - the target is parsed with `new URL()`; anything but http: or https: is
//     refused outright (no dialog, no open — a `javascript:` OSC 8 link included);
//   - a click outside ALLOWED_LINKS asks first, in a dialog showing `url.href`;
//   - the allowlist compares `url.origin` (scheme, host AND port) plus a path
//     prefix on `url.pathname`, never the href string, so
//     `https://github.com@evil.example/login/device` and `https://microsoft.com.evil/…`
//     match nothing;
//   - the console's own origin is never exempt: a link in terminal output must
//     not become a one-click console action.

/** Owner-approved one-click targets (term-t13): the device-login pages an agent's
 *  sign-in flow prints. Exact origin plus path prefix — a host alone is never enough. */
export const ALLOWED_LINKS: ReadonlyArray<{ origin: string; pathPrefix: string }> = [
  { origin: "https://microsoft.com", pathPrefix: "/devicelogin" },
  { origin: "https://github.com", pathPrefix: "/login/device" },
];

export type LinkDecision = { action: "refuse" } | { action: "open"; url: URL } | { action: "confirm"; url: URL };

// The prefix must end at a path boundary, so `/login/device-evil` is not `/login/device`.
function pathUnder(pathname: string, prefix: string): boolean {
  return pathname === prefix || pathname.startsWith(`${prefix}/`);
}

export function decideLink(raw: string, consoleOrigin: string): LinkDecision {
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    return { action: "refuse" };
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") return { action: "refuse" };
  const oneClick =
    url.origin !== consoleOrigin &&
    url.username === "" &&
    url.password === "" &&
    ALLOWED_LINKS.some((l) => url.origin === l.origin && pathUnder(url.pathname, l.pathPrefix));
  return oneClick ? { action: "open", url } : { action: "confirm", url };
}

export function openLink(url: URL): void {
  window.open(url.href, "_blank", "noopener,noreferrer");
}

/** The two xterm entry points, one policy. `confirm` receives the parsed target
 *  of every link that is neither refused nor allowlisted. */
export function terminalLinkHandlers(confirm: (url: URL) => void) {
  const activate = (_event: MouseEvent, uri: string) => {
    const d = decideLink(uri, window.location.origin);
    if (d.action === "open") openLink(d.url);
    else if (d.action === "confirm") confirm(d.url);
  };
  return {
    /** WebLinksAddon handler: URLs detected in the output text. */
    detected: activate,
    /** `Terminal` option `linkHandler`: OSC 8 links. */
    osc8: { activate, allowNonHttpProtocols: false },
  };
}
