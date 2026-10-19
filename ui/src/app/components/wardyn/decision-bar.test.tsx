/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DecisionBar } from "./decision-bar";

describe("DecisionBar", () => {
  it("says one decision needs you, then how many, and names the first request", () => {
    const { rerender } = render(<DecisionBar count={1} title="Push to main" onReview={() => {}} />);
    expect(screen.getByTestId("decision-bar")).toHaveTextContent("1 decision needs you · Push to main");
    rerender(<DecisionBar count={3} onReview={() => {}} />);
    expect(screen.getByTestId("decision-bar")).toHaveTextContent("3 decisions need you");
  });

  it("Review takes the person to the decisions", async () => {
    const onReview = vi.fn();
    render(<DecisionBar count={2} onReview={onReview} />);
    await userEvent.click(screen.getByRole("button", { name: "Review" }));
    expect(onReview).toHaveBeenCalledTimes(1);
  });
});
