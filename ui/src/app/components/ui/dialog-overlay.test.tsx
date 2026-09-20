/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import {
  AlertDialog,
  AlertDialogTrigger,
  AlertDialogContent,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogCancel,
} from "./alert-dialog";
import {
  Dialog,
  DialogTrigger,
  DialogContent,
  DialogTitle,
  DialogDescription,
  DialogClose,
} from "./dialog";

afterEach(() => vi.restoreAllMocks());

describe.each([
  {
    name: "AlertDialog",
    Root: AlertDialog,
    Trigger: AlertDialogTrigger,
    Content: AlertDialogContent,
    Title: AlertDialogTitle,
    Description: AlertDialogDescription,
    Cancel: AlertDialogCancel,
    slot: "alert-dialog-overlay",
  },
  {
    name: "Dialog",
    Root: Dialog,
    Trigger: DialogTrigger,
    Content: DialogContent,
    Title: DialogTitle,
    Description: DialogDescription,
    Cancel: DialogClose,
    slot: "dialog-overlay",
  },
])("$name", ({ Root, Trigger, Content, Title, Description, Cancel, slot }) => {
  function Confirmation() {
    return (
      <Root>
        <Trigger>Remove item</Trigger>
        <Content>
          <Title>Remove item?</Title>
          <Description>This cannot be undone.</Description>
          <Cancel>Cancel</Cancel>
        </Content>
      </Root>
    );
  }

  it("focuses cancel on open and returns focus to the trigger on close", async () => {
    const user = userEvent.setup();
    render(<Confirmation />);
    const trigger = screen.getByRole("button", { name: "Remove item" });
    await user.click(trigger);
    expect(screen.getByRole("button", { name: "Cancel" })).toHaveFocus();
    await user.keyboard("{Escape}");
    await waitFor(() => expect(trigger).toHaveFocus());
  });

  it("keeps the backdrop mounted until its close animation ends", async () => {
    // jsdom has no CSS animations; expose the live computed-style values that
    // Radix Presence reads as the existing animate-in/out classes change state.
    const realGetComputedStyle = window.getComputedStyle.bind(window);
    vi.spyOn(window, "getComputedStyle").mockImplementation((element) => {
      const styles = realGetComputedStyle(element);
      if (element.getAttribute("data-slot") === slot) {
        Object.defineProperty(styles, "animationName", {
          get: () => element.getAttribute("data-state") === "open"
            ? "enter"
            : "exit",
        });
      }
      return styles;
    });
    const user = userEvent.setup();
    render(<Confirmation />);
    await user.click(screen.getByRole("button", { name: "Remove item" }));
    const overlay = document.querySelector(`[data-slot="${slot}"]`)!;
    expect(overlay).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(overlay).toBeInTheDocument();
    expect(overlay).toHaveAttribute("data-state", "closed");
    const end = new Event("animationend", { bubbles: true });
    Object.defineProperty(end, "animationName", { value: "exit" });
    fireEvent(overlay, end);
    expect(overlay).not.toBeInTheDocument();
  });
});
