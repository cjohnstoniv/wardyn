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
