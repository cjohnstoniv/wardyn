/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// H2: safeReturnPath is the one gate between a 401's captured
// `window.location.pathname` and a post-auth `navigate(..., {replace:true})`.
// `internal/api/ui.go`'s catch-all serves index.html with no path cleaning, so
// `GET //evil.com` 200s and `window.location.pathname` reads back exactly
// `//evil.com` — a protocol-relative host a router's replaceState would dial
// cross-origin. Applied at BOTH capture (core.ts) and restore (App.tsx); this
// file pins the shared function both call.
import { describe, it, expect } from "vitest";
import { safeReturnPath } from "./core";

describe("safeReturnPath (H2)", () => {
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

  it("rejects the two landing-decision paths (root and /setup) and absent/empty input", () => {
    expect(safeReturnPath("/")).toBe("/runs");
    expect(safeReturnPath("/setup")).toBe("/runs");
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
