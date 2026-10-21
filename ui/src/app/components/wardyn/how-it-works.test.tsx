/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HowItWorks } from "./how-it-works";

// #3060: the longer explanation sits behind one closed disclosure that remembers its state per page.
describe("HowItWorks", () => {
  afterEach(() => window.localStorage.clear());

  it("starts closed and remembers being opened, per page", async () => {
    const { unmount } = render(
      <HowItWorks id="runners" title="How runners work">
        <p>Body</p>
      </HowItWorks>,
    );
    const block = screen.getByTestId("how-runners");
    expect(block).not.toHaveAttribute("open");
    await userEvent.setup().click(screen.getByText("How runners work"));
    expect(block).toHaveAttribute("open");
    unmount();

    render(
      <>
        <HowItWorks id="runners" title="How runners work">
          <p>Body</p>
        </HowItWorks>
        <HowItWorks id="governance" title="How governance works">
          <p>Other</p>
        </HowItWorks>
      </>,
    );
    expect(screen.getByTestId("how-runners")).toHaveAttribute("open");
    expect(screen.getByTestId("how-governance")).not.toHaveAttribute("open");
  });
});
