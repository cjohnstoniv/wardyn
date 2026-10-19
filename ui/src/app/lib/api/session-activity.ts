/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The console's half of session renewal (internal/auth/oidc/session_life.go). The server renews a
// session, and resets its idle clock, only for a request that is the person using the console, and a
// background poll is not one. So wfetch marks a request sent within ACTIVITY_WINDOW_MS of the
// person's own keyboard, pointer or wheel input. The session cookie is HttpOnly, so while renewal is
// on the server names the session's expiry on every answer instead: the shell's expiry banner and an
// open terminal read it here. With renewal off the server names none and nothing here changes.

export const ACTIVITY_HEADER = "X-Wardyn-Activity";
export const SESSION_EXPIRES_HEADER = "X-Wardyn-Session-Expires";
export const ACTIVITY_WINDOW_MS = 60_000;

let lastInput = 0;
const noteInput = () => {
  lastInput = Date.now();
};
const INPUT_EVENTS = ["keydown", "pointerdown", "wheel"] as const;
const INPUT_OPTIONS = { capture: true, passive: true } as const;

/** Count the person's own input, captured before any handler can stop it. Returns the stop function. */
export function listenForInput(): () => void {
  for (const kind of INPUT_EVENTS) window.addEventListener(kind, noteInput, INPUT_OPTIONS);
  return () => {
    for (const kind of INPUT_EVENTS) window.removeEventListener(kind, noteInput, INPUT_OPTIONS);
  };
}

/** Whether a request sent now is the person using the console. */
export function recentlyActive(): boolean {
  return lastInput > 0 && Date.now() - lastInput < ACTIVITY_WINDOW_MS;
}

let expiresAt: number | null = null;
// When the request that named `expiresAt` started. Answers arrive out of order, so a slower, older request must not
// move the expiry back; a LATER request that names an earlier expiry (a sign-out, a revoke) still does.
let namedBy = 0;
const expiryListeners = new Set<() => void>();

/**
 * Record the session expiry an answer named; an absent or unreadable value changes nothing, and neither does an
 * answer to a request that started before the one the current value came from. `requestedAt` is that start time.
 */
export function noteSessionExpiry(value: string | null | undefined, requestedAt: number = Date.now()): void {
  const at = value ? Date.parse(value) : NaN;
  if (Number.isNaN(at) || requestedAt < namedBy) return;
  namedBy = requestedAt;
  if (at === expiresAt) return;
  expiresAt = at;
  for (const listener of expiryListeners) listener();
}

/** The session expiry the server last named, in epoch milliseconds; null while it names none. */
export function sessionExpiry(): number | null {
  return expiresAt;
}

export function onSessionExpiry(fn: () => void): () => void {
  expiryListeners.add(fn);
  return () => {
    expiryListeners.delete(fn);
  };
}
