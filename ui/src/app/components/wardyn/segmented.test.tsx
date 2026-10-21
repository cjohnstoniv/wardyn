/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { Segmented } from "./segmented";

const OPTIONS = [
  { value: "git", label: "Git", dirty: true },
  { value: "storage", label: "Storage" },
];

describe("Segmented", () => {
  it("is a row of pressed-state buttons, and a click asks for that option", () => {
    const onChange = vi.fn();
    render(<Segmented value="git" options={OPTIONS} onChange={onChange} />);
    expect(screen.getByRole("button", { name: "Git" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "Storage" })).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(screen.getByRole("button", { name: "Storage" }));
    expect(onChange).toHaveBeenCalledWith("storage");
  });

  it("a dirty option shows its chip without changing the option's accessible name", () => {
    render(<Segmented value="git" options={OPTIONS} onChange={() => {}} />);
    // Exact-name lookups of the tab keep working while its draft is dirty.
    expect(screen.getByRole("button", { name: "Git" })).toBeInTheDocument();
    expect(screen.getByTestId("tab-dirty-chip-git")).toHaveAttribute("aria-hidden", "true");
    expect(screen.queryByTestId("tab-dirty-chip-storage")).toBeNull();
  });

  // #2870: an over-wide set scrolls inside its card rather than being clipped, and no label wraps.
  it("never wraps a label and scrolls inside itself rather than clipping", () => {
    render(<Segmented value="git" options={OPTIONS} onChange={() => {}} />);
    const git = screen.getByRole("button", { name: "Git" });
    expect(git).toHaveClass("whitespace-nowrap");
    const bar = git.parentElement!;
    expect(bar).toHaveClass("max-w-full", "overflow-x-auto");
    expect(bar).not.toHaveClass("overflow-hidden");
  });

  it("disables every option together", () => {
    render(<Segmented value="git" options={OPTIONS} onChange={() => {}} disabled />);
    for (const b of screen.getAllByRole("button")) expect(b).toBeDisabled();
  });

  it("the compact control keeps the dirty chip and the deny styling of the default one", () => {
    render(<Segmented size="compact" value="deny" options={[{ value: "permit", label: "Permit" }, { value: "deny", label: "Deny", dirty: true }]} onChange={() => {}} />);
    expect(screen.getByTestId("tab-dirty-chip-deny")).toHaveAttribute("aria-hidden", "true");
    expect(screen.getByRole("button", { name: "Deny" })).toHaveClass("text-danger");
    expect(screen.queryByTestId("tab-dirty-chip-permit")).toBeNull();
  });

  // #2832: the selected option carries the primary fill, not bg-muted (~1.1:1 against the card).
  it("the selected option is filled with the primary token and the others are not", () => {
    render(<Segmented value="git" options={OPTIONS} onChange={() => {}} />);
    const git = screen.getByRole("button", { name: "Git" });
    expect(git).toHaveClass("bg-primary", "text-primary-foreground");
    expect(git).not.toHaveClass("bg-muted");
    expect(screen.getByRole("button", { name: "Storage" })).not.toHaveClass("bg-primary");
  });

  // #2832: a dirty chip inside the selected (primary-filled) option sits on the solid card colour, not the 13% tint.
  it("the dirty chip on the selected option is solid, and on an unselected one keeps the warning tint", () => {
    render(<Segmented value="git" options={OPTIONS.map((o) => ({ ...o, dirty: true }))} onChange={() => {}} />);
    const on = screen.getByTestId("tab-dirty-chip-git").firstElementChild!;
    expect(on).toHaveClass("bg-card", "text-warning");
    expect(on).not.toHaveClass("bg-warning-subtle");
    const off = screen.getByTestId("tab-dirty-chip-storage").firstElementChild!;
    expect(off).toHaveClass("bg-warning-subtle");
    expect(off).not.toHaveClass("bg-card");
  });
});
