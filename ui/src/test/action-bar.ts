/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { expect } from "vitest";

// #3064, the one rule for a short form's action bar: actions at the right end, the one primary last, filled teal
// (it steps down to outline only where the page already gave its teal elsewhere), the secondary immediately
// before it, a destructive action at the far left in danger text, and the primary's label a verb and an object.
// jsdom has no layout, so the rule is pinned through the classes that make it.
export function expectActionBar(
  primary: HTMLElement,
  opts: { secondary?: HTMLElement; destructive?: HTMLElement; outlinePrimary?: boolean } = {},
): void {
  const bar = primary.parentElement!;
  expect(bar.className).toContain("justify-end");
  const buttons = Array.from(bar.querySelectorAll<HTMLElement>("button"));
  expect(buttons[buttons.length - 1]).toBe(primary);
  if (opts.outlinePrimary) expect(primary.className).toContain("border");
  else expect(primary.className).toContain("bg-primary");
  if (opts.secondary) {
    expect(buttons[buttons.indexOf(primary) - 1]).toBe(opts.secondary);
    expect(opts.secondary.className).not.toContain("bg-primary");
  }
  if (opts.destructive) {
    expect(buttons[0]).toBe(opts.destructive);
    expect(opts.destructive.className).toContain("mr-auto");
    expect(opts.destructive.className).toContain("text-danger");
  }
}
