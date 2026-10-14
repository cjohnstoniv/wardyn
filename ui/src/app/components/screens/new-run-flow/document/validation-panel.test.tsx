/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { NR_DOCUMENT as C } from "../../../wardyn/copy/new-run/document";
import { ValidationPanel } from "./validation-panel";

afterEach(cleanup);

describe("ValidationPanel", () => {
  it("says it is ready before any check, and valid after a clean one", () => {
    const { rerender } = render(<ValidationPanel id="s" status={{ kind: "ready" }} copy={C} />);
    expect(screen.getByText(C.READY)).toBeInTheDocument();
    rerender(<ValidationPanel id="s" status={{ kind: "valid" }} copy={C} />);
    expect(screen.getByText(C.VALID)).toBeInTheDocument();
  });

  it("says one issue in the singular and the plural line is unchanged", () => {
    const { rerender } = render(<ValidationPanel id="s" status={{ kind: "issues", count: 1, lines: ["one"] }} copy={C} />);
    expect(screen.getByText("1 issue must be fixed")).toBeInTheDocument();
    rerender(<ValidationPanel id="s" status={{ kind: "issues", count: 2, lines: ["one", "two"] }} copy={C} />);
    expect(screen.getByText("2 issues must be fixed")).toBeInTheDocument();
  });

  it("heads the problems with their count and lists only the lines it was given", () => {
    render(<ValidationPanel id="s" status={{ kind: "issues", count: 3, lines: ["one", "two"] }} copy={C} />);
    expect(screen.getByText(C.ISSUES(3))).toBeInTheDocument();
    expect(screen.getByText("one")).toBeInTheDocument();
    expect(screen.getByText("two")).toBeInTheDocument();
    expect(screen.getByTestId("validation-panel")).toHaveAttribute("id", "s");
  });
});
