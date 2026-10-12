/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Tooltip, HOVER_GRACE_MS } from "./tooltip";

// U14: a label-only bubble is hoverable, dismissible and never takes focus.
function setup() {
  const user = userEvent.setup({ delay: null });
  render(
    <Tooltip label="Copy the run id">
      <button type="button">Copy</button>
    </Tooltip>,
  );
  return { user, trigger: screen.getByRole("button", { name: "Copy" }) };
}
// Real timers: Testing Library's async wrapper never resolves under Vitest fake timers.
const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));
const SLOW = { timeout: 3000 };

describe("Tooltip", () => {
  it("opens at once on keyboard focus, as a tooltip the trigger is described by", async () => {
    const { user, trigger } = setup();
    expect(screen.queryByRole("tooltip")).toBeNull();
    await user.tab();
    expect(trigger).toHaveFocus();
    const tip = screen.getByRole("tooltip");
    expect(tip).toHaveTextContent("Copy the run id");
    expect(trigger).toHaveAttribute("aria-describedby", tip.id);
    expect(trigger).toHaveAccessibleDescription("Copy the run id");
    expect(trigger).not.toHaveAttribute("aria-haspopup");
  });

  it("Esc dismisses it and focus stays on the trigger", async () => {
    const { user, trigger } = setup();
    await user.tab();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("tooltip")).toBeNull();
    expect(trigger).toHaveFocus();
  });

  it("adds to the control's own description instead of replacing it", async () => {
    const user = userEvent.setup({ delay: null });
    render(
      <>
        <p id="why">Ask an admin to enable this.</p>
        <Tooltip label="Copy the run id">
          <button type="button" aria-disabled="true" aria-describedby="why">Copy</button>
        </Tooltip>
      </>,
    );
    const trigger = screen.getByRole("button", { name: "Copy" });
    // Opened by hover: jsdom's :focus-visible heuristic carries state between tests, hover does not.
    await user.hover(trigger);
    const tip = await screen.findByRole("tooltip", {}, SLOW);
    expect(trigger.getAttribute("aria-describedby")?.split(" ")).toEqual(["why", tip.id]);
    await user.keyboard("{Escape}");
    expect(trigger).toHaveAttribute("aria-describedby", "why");
  });

  it("opens only after the hover delay, and never takes focus", async () => {
    const { user, trigger } = setup();
    await user.hover(trigger);
    expect(screen.queryByRole("tooltip")).toBeNull();
    const tip = await screen.findByRole("tooltip", {}, SLOW);
    expect(trigger).not.toHaveFocus();
    expect(tip).not.toContainElement(document.activeElement as HTMLElement);
  });

  it("is hoverable: the pointer can move onto the bubble without it closing (WCAG 1.4.13)", async () => {
    const { user, trigger } = setup();
    await user.hover(trigger);
    const tip = await screen.findByRole("tooltip", {}, SLOW);
    await user.unhover(trigger);
    await user.hover(tip); // user-event refuses a pointer-events:none target
    await sleep(HOVER_GRACE_MS * 3);
    expect(screen.getByRole("tooltip")).toBeInTheDocument();
    await user.unhover(tip);
    await waitFor(() => expect(screen.queryByRole("tooltip")).toBeNull(), SLOW);
  });

  it("a mouse click that focuses the trigger does not open it (focus-visible only)", async () => {
    const { user, trigger } = setup();
    await user.click(trigger);
    expect(trigger).toHaveFocus();
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("leaves no timer behind when it unmounts mid-hover", () => {
    vi.useFakeTimers();
    try {
      const { unmount } = render(
        <Tooltip label="Copy the run id">
          <button type="button">Copy</button>
        </Tooltip>,
      );
      fireEvent.pointerEnter(screen.getByRole("button", { name: "Copy" }));
      expect(vi.getTimerCount()).toBe(1);
      unmount();
      expect(vi.getTimerCount()).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });
});
