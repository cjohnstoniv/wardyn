/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, beforeEach } from "vitest";
import { getToken, setToken } from "./core";

// Pins the admin-token storage posture (U098): the full-admin bearer defaults to
// sessionStorage (gone on browser close) and only reaches localStorage on an
// explicit opt-in. These pins FAIL if setToken reverts to unconditional
// localStorage persistence.
const KEY = "wardyn_admin_token";

describe("admin token storage posture", () => {
  beforeEach(() => {
    localStorage.clear();
    sessionStorage.clear();
  });

  it("default (no remember): persists to sessionStorage, NOT localStorage", () => {
    setToken("t-abc");
    expect(sessionStorage.getItem(KEY)).toBe("t-abc");
    expect(localStorage.getItem(KEY)).toBeNull();
    expect(getToken()).toBe("t-abc");
  });

  it("remember=true: persists to localStorage, clears sessionStorage", () => {
    setToken("t-abc", true);
    expect(localStorage.getItem(KEY)).toBe("t-abc");
    expect(sessionStorage.getItem(KEY)).toBeNull();
    expect(getToken()).toBe("t-abc");
  });

  it("migration: a token left in localStorage by an older build still resolves", () => {
    localStorage.setItem(KEY, "legacy");
    expect(getToken()).toBe("legacy");
  });

  it("sessionStorage wins over a stale localStorage copy", () => {
    localStorage.setItem(KEY, "old");
    setToken("fresh"); // default → sessionStorage, clears localStorage
    expect(getToken()).toBe("fresh");
    expect(localStorage.getItem(KEY)).toBeNull();
  });

  it("null clears both stores", () => {
    localStorage.setItem(KEY, "l");
    sessionStorage.setItem(KEY, "s");
    setToken(null);
    expect(localStorage.getItem(KEY)).toBeNull();
    expect(sessionStorage.getItem(KEY)).toBeNull();
    expect(getToken()).toBeNull();
  });
});
