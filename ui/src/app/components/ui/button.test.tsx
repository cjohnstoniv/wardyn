/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { Button } from "./button";

afterEach(cleanup);

// CONSOLE-RULES §2: the focus ring is full alpha. A /20 or /40 destructive ring never met the 3:1 floor, and
// twMerge lets a later ring colour replace the base one, so the variant must not carry its own.
describe("Button focus ring", () => {
  it.each(["default", "destructive", "outline", "ghost"] as const)("%s keeps the full-alpha ring", (variant) => {
    render(<Button variant={variant}>Go</Button>);
    const cls = screen.getByRole("button", { name: "Go" }).className.split(/\s+/);
    expect(cls).toContain("focus-visible:ring-ring");
    expect(cls.filter((c) => /ring-(?!ring\b|\[|offset)/.test(c) && /focus-visible:/.test(c))).toEqual([]);
  });
});

// #2865: in dark, a disabled filled button fading to 50% still reads as solid, so the filled variants swap to the
// muted pair. Outline, secondary, ghost and link carry a foreground colour, not a fill, and keep the plain fade.
describe("Button disabled treatment in dark", () => {
  it.each(["default", "destructive", "info"] as const)("%s turns grey in dark when disabled", (variant) => {
    render(<Button variant={variant} disabled>Go</Button>);
    const cls = screen.getByRole("button", { name: "Go" }).className.split(/\s+/);
    expect(cls).toContain("dark:disabled:bg-muted");
    expect(cls).toContain("dark:disabled:text-muted-foreground");
    expect(cls).toContain("disabled:opacity-50");
  });

  it.each(["outline", "secondary", "ghost", "link"] as const)("%s keeps the plain fade", (variant) => {
    render(<Button variant={variant} disabled>Go</Button>);
    const cls = screen.getByRole("button", { name: "Go" }).className.split(/\s+/);
    expect(cls.filter((c) => c.startsWith("dark:disabled:"))).toEqual([]);
    expect(cls).toContain("disabled:opacity-50");
  });
});
