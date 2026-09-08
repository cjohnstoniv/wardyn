/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// R4-F025 — THE PARITY GATE for the run-layout widget-id contract.
//
// internal/api/ui_layout.go's runLayoutWidgetIDs is a CLOSED set the server
// validates every PUT /api/v1/me/run-layout against (400 on an unknown id),
// and widget-registry.ts's WidgetId union is the other half. Both files say in
// their own doc comments that the halves "are kept in sync by hand" — and until
// this file, nothing checked. A console-only id passed `pnpm typecheck` and the
// whole run-detail suite; the human's cost lands at runtime instead:
// presetLayout() emits the id, useRunLayout.apply() PUTs it, the server 400s,
// and put() maps a 400 to "failed" (use-run-layout.ts), so the layout silently
// never persists behind a retry that can never succeed.
//
// Set EQUALITY, not containment, in the shape internal/db/migrations_check_test.go
// already uses for the agent_runs states: read the other language's source and
// compare. One direction is enough as long as it is equality.
import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { WIDGET_IDS } from "./widget-registry";

// Walk up from vitest's cwd (ui/) to the repo root rather than counting "../"s:
// a moved test file must not silently stop finding the Go half. import.meta.url
// is NOT a file: URL under vite's transform, so it cannot anchor this.
const GO_REL = "internal/api/ui_layout.go";
function goFilePath(): string {
  let dir = process.cwd();
  for (;;) {
    const candidate = resolve(dir, GO_REL);
    if (existsSync(candidate)) return candidate;
    const up = dirname(dir);
    if (up === dir) throw new Error(`widget-id parity: could not locate ${GO_REL} above ${process.cwd()}`);
    dir = up;
  }
}

/** The ids in `var runLayoutWidgetIDs = []string{...}`. Throws rather than
 *  returning [] when the declaration moves or is renamed: an empty set would
 *  make this gate pass on a file it no longer understands. */
function goWidgetIDs(): string[] {
  const file = goFilePath();
  const src = readFileSync(file, "utf8");
  const block = /var\s+runLayoutWidgetIDs\s*=\s*\[\]string\{([\s\S]*?)\}/.exec(src);
  if (!block) {
    throw new Error(
      `widget-id parity: could not find 'var runLayoutWidgetIDs = []string{...}' in ${file} — ` +
        "if the declaration was renamed or moved, update this gate in the SAME commit.",
    );
  }
  const ids = [...block[1].matchAll(/"([^"]+)"/g)].map((m) => m[1]);
  if (ids.length === 0) {
    throw new Error(`widget-id parity: runLayoutWidgetIDs parsed to an EMPTY set in ${file}`);
  }
  return ids;
}

describe("run-layout widget ids: the console registry and the server's closed set", () => {
  it("are the same set, exactly", () => {
    expect([...WIDGET_IDS].sort()).toEqual([...goWidgetIDs()].sort());
  });

  it("carry no duplicates on either side", () => {
    const go = goWidgetIDs();
    expect(new Set(go).size).toBe(go.length);
    expect(new Set(WIDGET_IDS).size).toBe(WIDGET_IDS.length);
  });
});
