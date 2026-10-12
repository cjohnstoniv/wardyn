/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { InfoTip } from "./info-tip";
import { HOVER_GRACE_MS } from "./tooltip";

// U14: the information a label cannot carry opens on tap and closes on Esc.
function setup() {
  const user = userEvent.setup({ delay: null });
  render(<InfoTip label="About Wall">The Wall keeps the agent off your network.</InfoTip>);
  return { user, trigger: screen.getByRole("button", { name: "About Wall" }) };
}
// Real timers: Testing Library's async wrapper never resolves under Vitest fake timers.
const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));
const SLOW = { timeout: 3000 };
const content = () => screen.queryByText("The Wall keeps the agent off your network.");

describe("InfoTip", () => {
  it("has an accessible name and is closed until used", () => {
    setup();
    expect(screen.getByRole("button", { name: "About Wall" })).toHaveAttribute("aria-expanded", "false");
    expect(content()).toBeNull();
  });

  it("a click or tap opens it without moving focus, and Esc closes it", async () => {
    const { user, trigger } = setup();
    await user.click(trigger);
    expect(content()).toBeInTheDocument();
    expect(trigger).toHaveAttribute("aria-expanded", "true");
    expect(trigger).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(content()).toBeNull();
  });

  it("Enter and Space open it from the keyboard", async () => {
    const { user } = setup();
    await user.tab();
    await user.keyboard("{Enter}");
    expect(content()).toBeInTheDocument();
    await user.keyboard("{Escape}");
    await user.keyboard(" ");
    expect(content()).toBeInTheDocument();
  });

  it("an outside click closes it", async () => {
    const { user, trigger } = setup();
    await user.click(trigger);
    await user.click(document.body);
    expect(content()).toBeNull();
  });

  it("opens on a hover pause, stays open while the pointer is on the bubble, and closes after leaving", async () => {
    const { user, trigger } = setup();
    await user.hover(trigger);
    expect(content()).toBeNull();
    const text = await screen.findByText("The Wall keeps the agent off your network.", {}, SLOW);
    await user.unhover(trigger);
    await user.hover(text); // user-event refuses a pointer-events:none target
    await sleep(HOVER_GRACE_MS * 3);
    expect(content()).toBeInTheDocument();
    await user.unhover(text);
    await waitFor(() => expect(content()).toBeNull(), SLOW);
  });

  it("a click on a hover-opened bubble pins it instead of closing it", async () => {
    const { user, trigger } = setup();
    await user.hover(trigger);
    await screen.findByText("The Wall keeps the agent off your network.", {}, SLOW);
    await user.click(trigger);
    await user.unhover(trigger);
    await sleep(HOVER_GRACE_MS * 3);
    expect(content()).toBeInTheDocument();
  });
});
