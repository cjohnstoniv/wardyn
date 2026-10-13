/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { BarrierMark } from "./barrier-mark";

describe("BarrierMark", () => {
  it("names the barrier as text in its metal colour and hides the glyph", () => {
    const { container } = render(<BarrierMark cc="CC2" />);
    expect(screen.getByText("Wall")).toHaveClass("text-wall-fg");
    expect(container.querySelector("svg")?.closest("[aria-hidden]")).not.toBeNull();
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
  });

  it("adds the strength strip only on request", () => {
    const { rerender } = render(<BarrierMark cc="CC3" strength />);
    expect(screen.getByRole("img", { name: "Barrier strength 3 of 3" })).toBeInTheDocument();
    rerender(<BarrierMark cc="CC3" />);
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
  });
});
