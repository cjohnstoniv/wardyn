/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "./dialog";

// jsdom does no layout, so this pins the classes that pin the bar; the e2e spec proves it on screen.
describe("DialogFooter", () => {
  it("is sticky to the bottom of the scrolling dialog, inside a form or not", () => {
    render(
      <Dialog open>
        <DialogContent>
          <DialogTitle>Edit</DialogTitle>
          <DialogDescription>Fields</DialogDescription>
          <form>
            <DialogFooter>
              <button type="submit">Save</button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>,
    );
    const footer = screen.getByRole("button", { name: "Save" }).parentElement!;
    expect(footer).toHaveAttribute("data-slot", "dialog-footer");
    expect(footer.className).toContain("sticky");
    expect(footer.className).toContain("bottom-0");
  });
});
