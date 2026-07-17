/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Private-mode-tolerant localStorage access — Safari private mode and SSR both
// throw on localStorage access; every caller here treats storage as best-effort.
export function lsGet(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

/** Set `key` to `value`, or remove it when `value` is null. No-op if storage is unavailable. */
export function lsSet(key: string, value: string | null): void {
  try {
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, value);
  } catch {
    /* localStorage unavailable (private mode / SSR) — ignore */
  }
}

// sessionStorage twins — same private-mode/SSR tolerance. Used for values that
// should NOT survive a browser restart by default (e.g. the admin bearer token,
// which persists to localStorage only on an explicit "remember on this device").
export function ssGet(key: string): string | null {
  try {
    return sessionStorage.getItem(key);
  } catch {
    return null;
  }
}

/** Set `key` to `value` in sessionStorage, or remove it when `value` is null. No-op if unavailable. */
export function ssSet(key: string, value: string | null): void {
  try {
    if (value === null) sessionStorage.removeItem(key);
    else sessionStorage.setItem(key, value);
  } catch {
    /* sessionStorage unavailable (private mode / SSR) — ignore */
  }
}
