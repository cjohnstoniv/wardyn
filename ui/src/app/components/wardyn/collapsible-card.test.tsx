/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1200 compact cards — collapsed by default, expands on click, and stays
// keyboard/aria correct. The mutation this pins: a card that opens by default
// breaks the 744px viewport e2e pin (settings-compact-viewport.spec.ts) — see
// that spec's own comment for the other half of this proof.
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { CollapsibleCard } from "./collapsible-card";
import { startsWith } from "../../lib/test-dom";

describe("CollapsibleCard", () => {
  it("collapsed by default: shows the title and summary, not the body", () => {
    render(
      <CollapsibleCard title="Host" summary="Runs default to Vault">
        <p>Full detail</p>
      </CollapsibleCard>,
    );
    expect(screen.getByRole("heading", { name: startsWith("Host"), level: 3 })).toBeVisible();
    expect(screen.getByText("Runs default to Vault")).toBeVisible();
    expect(screen.queryByText("Full detail")).not.toBeInTheDocument();
    expect(screen.getByRole("button")).toHaveAttribute("aria-expanded", "false");
  });

  // review FINAL-PR-1329-r2.md R2-M1: an `aria-label` on the heading is what
  // this card is not allowed to do — it makes NVDA/VoiceOver/TalkBack replace
  // the heading's announced content with the label, dropping the summary and
  // the expanded state from heading navigation even though the button's own
  // name (and Tab order) still carry them. The APG accordion example puts no
  // label on the `<h3>`; its name comes from its one child, the button.
  it("the heading takes its name from the button inside it, with no aria-label override", () => {
    render(
      <CollapsibleCard title="Host" summary="Runs default to Vault">
        <p>Full detail</p>
      </CollapsibleCard>,
    );
    const heading = screen.getByRole("heading", { level: 3 });
    expect(heading).not.toHaveAttribute("aria-label");
    // The summary is a genuine DESCENDANT of the heading, not swapped out by
    // a label — proven by finding it scoped inside the heading itself.
    expect(within(heading).getByText("Runs default to Vault")).toBeVisible();
    expect(screen.getByRole("heading", { name: startsWith("Host") })).toBe(heading);
  });

  it("a click expands the body and flips aria-expanded, another click collapses it", async () => {
    const user = userEvent.setup();
    render(
      <CollapsibleCard title="Host" summary="Runs default to Vault">
        <p>Full detail</p>
      </CollapsibleCard>,
    );
    const toggle = screen.getByRole("button");
    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText("Full detail")).toBeVisible();

    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText("Full detail")).not.toBeInTheDocument();
  });

  it("the toggle button owns the disclosure from the keyboard alone (Enter), no separate control needed", async () => {
    const user = userEvent.setup();
    render(
      <CollapsibleCard title="Host">
        <p>Full detail</p>
      </CollapsibleCard>,
    );
    await user.tab();
    expect(screen.getByRole("button")).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(screen.getByText("Full detail")).toBeVisible();
  });

  it("defaultOpen renders expanded from the start — the opt-in a caller must never make the page-wide default", () => {
    render(
      <CollapsibleCard title="Host" defaultOpen>
        <p>Full detail</p>
      </CollapsibleCard>,
    );
    expect(screen.getByRole("button")).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText("Full detail")).toBeVisible();
  });

  it("controlled open + onOpenChange: the caller decides, this component only reports the click", async () => {
    const user = userEvent.setup();
    const onOpenChange = vi.fn();
    const { rerender } = render(
      <CollapsibleCard title="Azure DevOps" open={false} onOpenChange={onOpenChange}>
        <p>Full detail</p>
      </CollapsibleCard>,
    );
    await user.click(screen.getByRole("button"));
    expect(onOpenChange).toHaveBeenCalledWith(true);
    // The click alone never opens it — a controlled card only reflects the prop.
    expect(screen.queryByText("Full detail")).not.toBeInTheDocument();

    rerender(
      <CollapsibleCard title="Azure DevOps" open onOpenChange={onOpenChange}>
        <p>Full detail</p>
      </CollapsibleCard>,
    );
    expect(screen.getByText("Full detail")).toBeVisible();
  });
});
