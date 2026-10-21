/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { PageHeader } from "./page-header";

// #2831: the phone layout (title and action on one row, description under both) is a layout fact, pinned in
// ui/e2e/console-ux-tier-b-admin.spec.ts. What jsdom can pin is the contract that layout rides on: the
// description and the actions are addressable, and the DOM order is the one desktop has always had.
describe("PageHeader", () => {
  it("keeps the title, the description and the actions in that DOM order, each addressable", () => {
    render(<PageHeader title="Policies" description="What a run may do." actions={<button type="button">New policy</button>} />);
    const title = screen.getByRole("heading", { name: "Policies" });
    const description = screen.getByTestId("page-header-description");
    const actions = screen.getByTestId("page-header-actions");
    expect(description).toHaveTextContent("What a run may do.");
    expect(actions).toContainElement(screen.getByRole("button", { name: "New policy" }));
    expect(title.compareDocumentPosition(description) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(description.compareDocumentPosition(actions) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("draws the page title on the page-title rung (#3080), and a nested pane's h3 on the body rung", () => {
    const { unmount } = render(<PageHeader title="Policies" />);
    expect(screen.getByRole("heading", { level: 1, name: "Policies" })).toHaveClass("text-page");
    unmount();
    render(<PageHeader title="SSH keys" as="h3" />);
    expect(screen.getByRole("heading", { level: 3, name: "SSH keys" })).not.toHaveClass("text-page");
  });

  it("draws no description or actions slot when it has none", () => {
    render(<PageHeader title="Policies" />);
    expect(screen.queryByTestId("page-header-description")).toBeNull();
    expect(screen.queryByTestId("page-header-actions")).toBeNull();
  });
});
