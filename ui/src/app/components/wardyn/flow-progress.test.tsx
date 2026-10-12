/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { FlowProgress } from "./flow-progress";

const STEPS = [
  { id: "a", label: "Shape", state: "done" as const },
  { id: "b", label: "Files", state: "current" as const },
  { id: "c", label: "Review", state: "upcoming" as const },
];

describe("FlowProgress", () => {
  it("is a named nav with an ordered list and exactly one current step", () => {
    render(<FlowProgress steps={STEPS} label="Progress" />);
    const list = within(screen.getByRole("navigation", { name: "Progress" })).getByRole("list");
    const items = within(list).getAllByRole("listitem");
    expect(items).toHaveLength(3);
    expect(items.filter((i) => i.getAttribute("aria-current") === "step")).toHaveLength(1);
    expect(items[1]).toHaveAttribute("aria-current", "step");
  });

  it("keeps every label in the accessibility tree, only visually hidden below sm", () => {
    render(<FlowProgress steps={STEPS} label="Progress" />);
    for (const s of STEPS) expect(screen.getByText(s.label)).toHaveClass("sr-only", "sm:not-sr-only");
  });
});
