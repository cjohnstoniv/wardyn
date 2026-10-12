/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { DOCUMENT_COPY as C } from "./copy-seam";
import { ValidationPanel } from "./validation-panel";

afterEach(cleanup);

describe("ValidationPanel", () => {
  it("says it is ready before any check, and valid after a clean one", () => {
    const { rerender } = render(<ValidationPanel id="s" status={{ kind: "ready" }} copy={C} />);
    expect(screen.getByText(C.ready)).toBeInTheDocument();
    rerender(<ValidationPanel id="s" status={{ kind: "valid" }} copy={C} />);
    expect(screen.getByText(C.valid)).toBeInTheDocument();
  });

  it("heads the problems with their count and lists only the lines it was given", () => {
    render(<ValidationPanel id="s" status={{ kind: "issues", count: 3, lines: ["one", "two"] }} copy={C} />);
    expect(screen.getByText(C.issues(3))).toBeInTheDocument();
    expect(screen.getByText("one")).toBeInTheDocument();
    expect(screen.getByText("two")).toBeInTheDocument();
    expect(screen.getByTestId("validation-panel")).toHaveAttribute("id", "s");
  });
});
