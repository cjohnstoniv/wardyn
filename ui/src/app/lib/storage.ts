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
