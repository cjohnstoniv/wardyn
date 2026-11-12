/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The desktop stream's close codes are the gateway's (internal/api/uigateway_desktop.go) and the viewer's
// (viewer.tsx) to agree on. This reads both sources by path, as wire-parity.test.ts does, so a code added on
// one side, such as the window-open close, cannot go unmapped on the other.
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { WINDOW_OPEN } from "./viewer";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

describe("desktop close codes, gateway vs viewer", () => {
  const root = repoRoot();
  const go = readFileSync(join(root, "internal/api/uigateway_desktop.go"), "utf8");
  const ts = readFileSync(join(root, "ui/src/desktop/viewer.tsx"), "utf8");
  const goCodes = new Map([...go.matchAll(/\b(desktopClose\w+)\s+websocket\.StatusCode = (\d+)/g)].map((m) => [m[1], Number(m[2])]));
  const tsCodes = [...ts.matchAll(/^(?:export )?const [A-Z_]+ = (40\d\d);/gm)].map((m) => Number(m[1]));

  it("the viewer maps every code the gateway closes with, and no other", () => {
    expect(goCodes.size).toBeGreaterThanOrEqual(6);
    expect([...new Set(tsCodes)].sort()).toEqual([...new Set(goCodes.values())].sort());
  });

  it("a window open on the Mac is 4006 on both sides", () => {
    expect(goCodes.get("desktopCloseWindowOpen")).toBe(WINDOW_OPEN);
    expect(WINDOW_OPEN).toBe(4006);
  });
});
