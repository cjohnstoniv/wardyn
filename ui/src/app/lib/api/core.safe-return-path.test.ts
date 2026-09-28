/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// H2: safeReturnPath is the one gate between the page's pathname and the
// fresh reload a sign-in by someone else triggers (App.tsx's
// window.location.assign, #483). `internal/api/ui.go`'s catch-all serves
// index.html with no path cleaning, so `GET //evil.com` 200s and
// `window.location.pathname` reads back exactly `//evil.com` — a
// protocol-relative host a navigation would dial cross-origin. This file pins
// the function; App.reauth.test.tsx pins its use at the reload.
import { describe, it, expect } from "vitest";
import { safeReturnPath } from "./core";

describe("safeReturnPath", () => {
  // ticket: H2
  it("rejects a protocol-relative host (//host)", () => {
    expect(safeReturnPath("//evil.com")).toBe("/runs");
    expect(safeReturnPath("//evil.com/x")).toBe("/runs");
  });

  it("rejects a backslash host (/\\host — some browsers normalize \\ to /)", () => {
    expect(safeReturnPath("/\\evil.com")).toBe("/runs");
  });

  it("rejects an absolute URL with a scheme", () => {
    expect(safeReturnPath("https://evil.com/x")).toBe("/runs");
  });

  it("rejects the landing-decision paths (root and setup, in either view) and absent/empty input", () => {
    expect(safeReturnPath("/")).toBe("/runs");
    expect(safeReturnPath("/setup")).toBe("/runs");
    expect(safeReturnPath("/admin")).toBe("/runs");
    expect(safeReturnPath("/admin/setup")).toBe("/runs");
    expect(safeReturnPath(null)).toBe("/runs");
    expect(safeReturnPath(undefined)).toBe("/runs");
    expect(safeReturnPath("")).toBe("/runs");
  });

  // Negative control: a normal same-origin pathname, with a query string,
  // restores verbatim.
  it("neg: a normal same-origin path (with a query string) restores unchanged", () => {
    expect(safeReturnPath("/drives?x=1")).toBe("/drives?x=1");
    expect(safeReturnPath("/workspaces/abc-123")).toBe("/workspaces/abc-123");
  });
});
