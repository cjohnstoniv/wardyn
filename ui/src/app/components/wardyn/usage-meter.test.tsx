/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { UsageMeter } from "./usage-meter";

describe("UsageMeter", () => {
  it("is a meter with value, bounds and text, and the text is on screen", () => {
    render(<UsageMeter label="Pool" used={3} max={10} valueText="3 of 10 in use" />);
    const m = screen.getByRole("meter", { name: "Pool" });
    expect(m).toHaveAttribute("aria-valuenow", "3");
    expect(m).toHaveAttribute("aria-valuemin", "0");
    expect(m).toHaveAttribute("aria-valuemax", "10");
    expect(m).toHaveAttribute("aria-valuetext", "3 of 10 in use");
    expect(screen.getByText("3 of 10 in use")).toBeVisible();
    expect(m.firstElementChild).toHaveClass("bg-info");
    expect(m.firstElementChild).toHaveStyle({ width: "30%" });
  });

  it("turns the fill to warning at the cap and keeps the text", () => {
    render(<UsageMeter label="Pool" used={10} max={10} valueText="10 of 10 in use" />);
    expect(screen.getByRole("meter").firstElementChild).toHaveClass("bg-warning");
    expect(screen.getByText("10 of 10 in use")).toBeVisible();
  });

  it("with no max it is text only: no meter, no bar", () => {
    const { container } = render(<UsageMeter label="Pool" used={3} valueText="Set by its runners" />);
    expect(screen.queryByRole("meter")).toBeNull();
    expect(screen.getByText("Set by its runners")).toBeVisible();
    expect(container.querySelector(".bg-muted")).toBeNull();
  });

  it("never reports a value above its max; the text keeps the true figure", () => {
    render(<UsageMeter label="Pool" used={12} max={10} valueText="12 of 10 in use" />);
    const m = screen.getByRole("meter");
    expect(m).toHaveAttribute("aria-valuenow", "10");
    expect(m).toHaveAttribute("aria-valuetext", "12 of 10 in use");
  });
});
