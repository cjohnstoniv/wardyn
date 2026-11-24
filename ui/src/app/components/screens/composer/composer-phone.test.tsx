/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import { PhoneDraftBar, PhoneDraftSheet, useIsPhone } from "./composer-phone";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function Probe() {
  return <p data-testid="phone">{String(useIsPhone())}</p>;
}
const width = (w: number) => Object.defineProperty(window, "innerWidth", { configurable: true, writable: true, value: w });

describe("useIsPhone", () => {
  it.each([
    [390, "true"],
    [759, "true"],
    [760, "false"],
    [1280, "false"],
  ])("answers from the window width at %i px when there is no media query", (w, answer) => {
    vi.stubGlobal("matchMedia", undefined);
    width(w);
    render(<Probe />);
    expect(screen.getByTestId("phone")).toHaveTextContent(answer);
  });

  it("answers from the media query when the browser has one", () => {
    vi.stubGlobal("matchMedia", () => ({ matches: true, addEventListener() {}, removeEventListener() {} }));
    width(1280);
    render(<Probe />);
    expect(screen.getByTestId("phone")).toHaveTextContent("true");
  });
});

describe("the bar and the sheet", () => {
  it("shows the draft's size and opens the sheet from Show draft", async () => {
    const onShow = vi.fn();
    render(<PhoneDraftBar lines={7} left={1} onShow={onShow} />);
    expect(screen.getByText(T.DRAFT_BAR(7, 1))).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: T.SHOW_DRAFT }));
    expect(onShow).toHaveBeenCalledTimes(1);
  });

  it("renders its children only while open, and closes from its own button", async () => {
    const onClose = vi.fn();
    const { rerender } = render(
      <PhoneDraftSheet open={false} onClose={onClose}>
        <p>draft</p>
      </PhoneDraftSheet>,
    );
    expect(screen.queryByText("draft")).toBeNull();
    rerender(
      <PhoneDraftSheet open onClose={onClose}>
        <p>draft</p>
      </PhoneDraftSheet>,
    );
    expect(screen.getByRole("dialog")).toContainElement(screen.getByText("draft"));
    await userEvent.click(screen.getByRole("button", { name: T.HIDE_DRAFT }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
