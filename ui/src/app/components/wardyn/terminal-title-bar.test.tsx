/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { TerminalTitleBar } from "./terminal-title-bar";

afterEach(cleanup);

describe("TerminalTitleBar", () => {
  it("is a dark-scoped bar with three dots, a muted title, then the caller's controls", () => {
    render(
      <TerminalTitleBar title="bash">
        <button>x</button>
      </TerminalTitleBar>,
    );
    const bar = screen.getByTestId("terminal-title-bar");
    expect(bar).toHaveClass("dark", "bg-terminal-chrome", "border-b");
    expect(bar.querySelector(".bg-terminal-dot-close")).not.toBeNull();
    expect(bar.querySelector(".bg-terminal-dot-minimize")).not.toBeNull();
    expect(bar.querySelector(".bg-terminal-dot-zoom")).not.toBeNull();
    const title = screen.getByText("bash");
    expect(title).toHaveClass("text-terminal-chrome-foreground");
    expect(title).not.toHaveClass("text-muted-foreground");
    const button = screen.getByRole("button", { name: "x" });
    expect(title.compareDocumentPosition(button) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("has no title span without a title", () => {
    render(<TerminalTitleBar />);
    expect(screen.getByTestId("terminal-title-bar").querySelector(".font-mono")).toBeNull();
  });
});
