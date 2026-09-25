/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

// F7-F13 — 39 toast.error() call sites across 28 files, none passing an
// explicit duration; the ONE place to raise the default (and turn on a
// manual-dismiss affordance) without touching every call site is the shared
// <Toaster> wrapper every mount of the app renders (App.tsx, sign-in.tsx).
// Source-scan, the same discipline theme-contrast.test.ts / tabs.test.tsx use
// for a "use client" wrapper whose only job is to set props on a vendored
// primitive — no Radix/portal rendering ceremony needed to pin two prop values.
describe("Toaster — closeButton + a longer default duration", () => {
  // ticket: F7-F13
  const src = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "sonner.tsx"), "utf8");

  it("defaults closeButton to true (a manual-dismiss affordance, not auto-dismiss-only)", () => {
    expect(src).toMatch(/closeButton(?:=\{true\}|\s*\n)/);
  });

  it("raises the default toast duration above sonner's stock 4000ms", () => {
    const m = /toastOptions=\{\{[^}]*duration:\s*(\d+)/.exec(src);
    expect(m, "toastOptions.duration not found").not.toBeNull();
    expect(Number(m![1])).toBeGreaterThan(4000);
  });

  // A caller-supplied prop must still win — <Toaster closeButton={false} />
  // or a per-call toast.error(msg, {duration}) both need to keep working.
  it("props spread AFTER the defaults, so a caller can still override either", () => {
    // lastIndexOf: the FIRST `{...props}` in the file is the `({ ...props })`
    // destructure in the component's own parameter list, not the JSX spread.
    const propsSpreadIdx = src.lastIndexOf("{...props}");
    const closeButtonIdx = src.indexOf("closeButton");
    const toastOptionsIdx = src.indexOf("toastOptions");
    expect(propsSpreadIdx).toBeGreaterThan(-1);
    expect(closeButtonIdx).toBeLessThan(propsSpreadIdx);
    expect(toastOptionsIdx).toBeLessThan(propsSpreadIdx);
  });
});
