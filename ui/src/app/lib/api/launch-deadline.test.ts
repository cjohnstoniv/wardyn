/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// P5 rider — the CLASS, not the one call: "a console call that launches a
// sandbox inherits a 60s deadline the server may legitimately exceed".
//
// wfetch's default (WFETCH_TIMEOUT_MS) bounds a HANG; it is not a latency
// budget. Every one of these paths blocks through CreateSandbox server-side,
// which on k8s waits canaryWaitTimeout on top of a cold image pull — so the
// default turned a launch that was working into "Could not reach the control
// plane." and threw away the id the console was about to be handed.
//
// A SOURCE SCAN rather than a behavioural test, deliberately: the defect is a
// missing third argument at a call site, and the thing that must not regress is
// that NO launch call site is left on the default. A new launch path added to
// this list with no deadline reds here. core.deadline.test.ts (wfetch's own
// deadline behaviour) is untouched by this and stays the contract underneath.
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, it, expect } from "vitest";
import { LAUNCH_DEADLINE_MS, WFETCH_TIMEOUT_MS } from "./core";

const here = dirname(fileURLToPath(import.meta.url));
const read = (f: string) => readFileSync(join(here, f), "utf8");

// Every console call whose server side brings a sandbox up, and the module it
// lives in. POST /setup/harness-login is NOT here: it answers before dispatch
// now (internal/api/harnesscred_launch.go), so it is a fast call again.
const LAUNCH_CALLS: ReadonlyArray<{ file: string; fn: string; path: string }> = [
  { file: "runs.ts", fn: "async createRun(", path: '"/runs"' },
  { file: "runs.ts", fn: "async preflightRun(", path: '"/runs/preflight"' },
  { file: "workspaces.ts", fn: "async recordTask(", path: "/record`" },
];

describe("every console call that launches a sandbox passes an explicit deadline", () => {
  for (const { file, fn, path } of LAUNCH_CALLS) {
    it(`${file} ${fn.replace("async ", "")}) does not ride the 60s default`, () => {
      const src = read(file);
      const start = src.indexOf(fn);
      expect(start, `no ${fn} in ${file}`).toBeGreaterThan(-1);
      // The method's own body: from its signature to the wfetch call's closing
      // `);`. The deadline is wfetch's third argument, so it lands inside.
      const at = src.indexOf(path, start);
      expect(at, `${fn} in ${file} no longer calls ${path}`).toBeGreaterThan(-1);
      const call = src.slice(start, src.indexOf(");", at));
      expect(call).toContain("LAUNCH_DEADLINE_MS");
    });
  }

  it("the launch deadline is longer than the default it replaces, and finite", () => {
    expect(LAUNCH_DEADLINE_MS).toBeGreaterThan(WFETCH_TIMEOUT_MS);
    expect(Number.isFinite(LAUNCH_DEADLINE_MS)).toBe(true);
  });
});
