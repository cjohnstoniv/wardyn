/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import {
  Sheet, SheetTrigger, SheetContent, SheetTitle, SheetDescription,
} from "./sheet";

afterEach(() => vi.restoreAllMocks());

function Navigation({ side = "right" }: { side?: "top" | "right" | "bottom" | "left" }) {
  return (
    <Sheet>
      <SheetTrigger>Open navigation</SheetTrigger>
      <SheetContent side={side}>
        <SheetTitle>Navigation</SheetTitle>
        <SheetDescription>Choose a page.</SheetDescription>
        <button>Runs</button>
      </SheetContent>
    </Sheet>
  );
}

describe("Sheet overlay", () => {
  it.each(["Escape", "Close"])("preserves focus when dismissed with %s", async (method) => {
    const user = userEvent.setup();
    render(<Navigation />);
    const trigger = screen.getByRole("button", { name: "Open navigation" });
    await user.click(trigger);
    expect(screen.getByRole("dialog", { name: "Navigation" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Runs" })).toHaveFocus();
    if (method === "Escape") await user.keyboard("{Escape}");
    else await user.click(screen.getByRole("button", { name: "Close" }));
    await waitFor(() => expect(trigger).toHaveFocus());
  });

  it.each(["top", "right", "bottom", "left"] as const)(
    "retains the %s sheet backdrop until its exit animation ends",
    async (side) => {
      // jsdom has no CSS animations; supply the live animation names that
      // Radix Presence reads from the existing state-dependent classes.
      const realGetComputedStyle = window.getComputedStyle.bind(window);
      vi.spyOn(window, "getComputedStyle").mockImplementation((element) => {
        const styles = realGetComputedStyle(element);
        if (element.getAttribute("data-slot") === "sheet-overlay") {
          Object.defineProperty(styles, "animationName", {
            get: () => element.getAttribute("data-state") === "open" ? "enter" : "exit",
          });
        }
        return styles;
      });
      const user = userEvent.setup();
      render(<Navigation side={side} />);
      await user.click(screen.getByRole("button", { name: "Open navigation" }));
      const overlay = document.querySelector('[data-slot="sheet-overlay"]')!;
      expect(overlay).toBeInTheDocument();
      await user.click(screen.getByRole("button", { name: "Close" }));
      expect(overlay).toBeInTheDocument();
      expect(overlay).toHaveAttribute("data-state", "closed");
      const end = new Event("animationend", { bubbles: true });
      Object.defineProperty(end, "animationName", { value: "exit" });
      fireEvent(overlay, end);
      expect(overlay).not.toBeInTheDocument();
    },
  );
});
