/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RadioCard, RadioCardGroup } from "./radio-card";

// U15: a card that cannot be chosen says why in words, and the radio carries that
// reason as its description.
const REASON = "Needs a runner with a Vault barrier.";

function Group({ layout = "card", onValueChange = () => {} }: { layout?: "card" | "row"; onValueChange?: (v: string) => void }) {
  return (
    <>
      <h2 id="g">Barrier</h2>
      <RadioCardGroup value="fence" onValueChange={onValueChange} name="barrier" aria-labelledby="g" layout={layout}>
        <RadioCard value="fence" title="Fence" description="Weakest" meta="Fastest" />
        <RadioCard value="wall" title="Wall" description="Stronger" badge={<span>Default</span>} />
        <RadioCard value="vault" title="Vault" description="Strongest" unavailable={{ reason: REASON }} />
      </RadioCardGroup>
    </>
  );
}

describe("RadioCard", () => {
  it("is one named radiogroup whose cards are named by their title alone", () => {
    render(<Group />);
    expect(screen.getByRole("radiogroup", { name: "Barrier" })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Fence" })).toBeChecked();
    expect(screen.getByRole("radio", { name: "Wall" })).not.toBeChecked();
  });

  it("an unavailable card shows its reason as visible text and in its accessible description", () => {
    render(<Group />);
    expect(screen.getByText(REASON)).toBeVisible();
    const vault = screen.getByRole("radio", { name: "Vault" });
    expect(vault).toBeDisabled();
    expect(vault).toHaveAccessibleDescription(expect.stringContaining(REASON));
    expect(vault.closest("label")).toHaveClass("hatch-unavailable");
  });

  it("description, meta and badge are part of a card's description", () => {
    render(<Group />);
    expect(screen.getByRole("radio", { name: "Fence" })).toHaveAccessibleDescription("Weakest Fastest");
    expect(screen.getByRole("radio", { name: "Wall" })).toHaveAccessibleDescription("Stronger Default");
  });

  it("an arrow key moves to the next card and chooses it; the unavailable card is skipped", async () => {
    const onValueChange = vi.fn();
    const user = userEvent.setup();
    render(<Group onValueChange={onValueChange} />);
    screen.getByRole("radio", { name: "Fence" }).focus();
    // Held, because Radix moves focus on a timer and only selects while the arrow is still down.
    await user.keyboard("{ArrowDown>}");
    await waitFor(() => expect(onValueChange).toHaveBeenLastCalledWith("wall"));
    await user.keyboard("{/ArrowDown}");
    expect(screen.getByRole("radio", { name: "Wall" })).toHaveFocus();
    await user.keyboard("{ArrowDown>}");
    await waitFor(() => expect(screen.getByRole("radio", { name: "Fence" })).toHaveFocus());
    await user.keyboard("{/ArrowDown}");
    expect(screen.getByRole("radio", { name: "Vault" })).not.toHaveFocus();
  });

  it("clicking a card's text chooses it; clicking the unavailable one does not", async () => {
    const onValueChange = vi.fn();
    const user = userEvent.setup();
    render(<Group onValueChange={onValueChange} />);
    await user.click(screen.getByText("Stronger"));
    expect(onValueChange).toHaveBeenCalledWith("wall");
    onValueChange.mockClear();
    await user.click(screen.getByText("Strongest"));
    expect(onValueChange).not.toHaveBeenCalled();
  });

  it("the row layout is the same group with a decorative chevron", () => {
    const { container } = render(<Group layout="row" />);
    expect(screen.getAllByRole("radio")).toHaveLength(3);
    expect(container.querySelectorAll("svg[aria-hidden='true']").length).toBeGreaterThanOrEqual(3);
  });
});
