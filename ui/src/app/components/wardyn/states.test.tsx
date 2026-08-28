/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ErrorState } from "./states";

describe("ErrorState — action", () => {
  it("renders a passed action after the message", async () => {
    const onClick = vi.fn();
    render(<ErrorState action={<button onClick={onClick}>Do the thing</button>} />);
    await userEvent.click(screen.getByRole("button", { name: "Do the thing" }));
    expect(onClick).toHaveBeenCalledTimes(1);
    // No onRetry was passed, so the built-in Retry button stays absent.
    expect(screen.queryByRole("button", { name: /retry/i })).not.toBeInTheDocument();
  });

  it("still renders the built-in Retry button when onRetry is passed instead", () => {
    render(<ErrorState onRetry={() => {}} />);
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
  });
});
