/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { EmptyState, ErrorState } from "./states";

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

// F7-F10 — default heading level. A hardcoded <h3> was correct only for a
// caller sitting under a real h1+h2; several don't (an EmptyState/ErrorState
// that IS the whole visible content of a section), which skipped a heading
// level (WCAG 1.3.1/2.4.6). `as` defaults to h2, the safer floor.
describe("EmptyState / ErrorState — heading level (F7-F10)", () => {
  it("EmptyState's title is an h2 by default", () => {
    render(<EmptyState icon={() => null} title="Nothing here yet" />);
    expect(screen.getByRole("heading", { name: "Nothing here yet", level: 2 })).toBeInTheDocument();
  });

  it("EmptyState honors an explicit `as` override", () => {
    render(<EmptyState icon={() => null} title="Nothing here yet" as="h3" />);
    expect(screen.getByRole("heading", { name: "Nothing here yet", level: 3 })).toBeInTheDocument();
  });

  it("ErrorState's title is an h2 by default", () => {
    render(<ErrorState />);
    expect(screen.getByRole("heading", { name: "Something went wrong", level: 2 })).toBeInTheDocument();
  });

  it("ErrorState honors an explicit `as` override", () => {
    render(<ErrorState as="h3" />);
    expect(screen.getByRole("heading", { name: "Something went wrong", level: 3 })).toBeInTheDocument();
  });
});
