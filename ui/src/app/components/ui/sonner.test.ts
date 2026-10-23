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
    expect(src).toMatch(/^\s+closeButton(?:=\{true\})?\s*$/m);
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
    // The JSX attributes, not the first mention: the comment above them names both props too.
    const closeButtonIdx = /^\s+closeButton(?:=\{true\})?\s*$/m.exec(src)?.index ?? -1;
    const toastOptionsIdx = src.indexOf("toastOptions={{");
    expect(closeButtonIdx).toBeGreaterThan(-1);
    expect(toastOptionsIdx).toBeGreaterThan(-1);
    expect(propsSpreadIdx).toBeGreaterThan(-1);
    expect(closeButtonIdx).toBeLessThan(propsSpreadIdx);
    expect(toastOptionsIdx).toBeLessThan(propsSpreadIdx);
  });

  // #3385 — a toast at the bottom right covered Save providers for 8 s. Top centre, offset below the top bar
  // (top-bar.tsx is h-14 = 56px), so it covers page-title text and never a control.
  it("opens at the top centre, offset below the 56px top bar, on desktop and phone alike", () => {
    expect(src).toMatch(/position="top-center"/);
    const off = /\boffset=\{\{\s*top:\s*(\d+)/.exec(src);
    const mob = /mobileOffset=\{\{\s*top:\s*(\d+)/.exec(src);
    expect(off, "offset.top not found").not.toBeNull();
    expect(mob, "mobileOffset.top not found").not.toBeNull();
    expect(Number(off![1])).toBeGreaterThanOrEqual(56);
    expect(Number(mob![1])).toBeGreaterThanOrEqual(56);
    // Placed before the {...props} spread, so a caller can still override.
    expect(src.indexOf('position="top-center"')).toBeLessThan(src.lastIndexOf("{...props}"));
  });
});
