/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { HostList, type HostRow } from "./host-list";

const row = (host: string): HostRow => ({ host, provenance: "approved for this workspace", removable: true });

// Each removal PUTs the whole list built from the render-time workspace, so a second removal
// started before the first returns would write the first host back (#2581).
describe("HostList — one removal at a time", () => {
  it("disables every row's remove button while any removal is in flight", () => {
    render(<HostList rows={[row("a.example"), row("b.example")]} emptyText="none" canRemove removing="a.example" onRemove={vi.fn()} />);
    expect(screen.getByRole("button", { name: "Remove a.example" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove b.example" })).toBeDisabled();
  });

  it("leaves them live when nothing is being removed", () => {
    render(<HostList rows={[row("a.example"), row("b.example")]} emptyText="none" canRemove removing={null} onRemove={vi.fn()} />);
    expect(screen.getByRole("button", { name: "Remove b.example" })).toBeEnabled();
  });
});

// CONSOLE-RULES: a disabled control says why in visible text; `title` alone is never the reason (#2584).
describe("HostList — a blocked removal says why", () => {
  it("shows the reason as text in the row and as the button's description, and sets no title", () => {
    const blocked: HostRow = { ...row("a.example"), removeBlockedReason: "Only a security admin can remove this." };
    render(<HostList rows={[blocked, row("b.example")]} emptyText="none" canRemove removing={null} onRemove={vi.fn()} />);
    expect(screen.getByText("Only a security admin can remove this.")).toBeVisible();
    const button = screen.getByRole("button", { name: "Remove a.example" });
    expect(button).toBeDisabled();
    expect(button).toHaveAccessibleDescription("Only a security admin can remove this.");
    expect(button).not.toHaveAttribute("title");
    expect(screen.getByRole("button", { name: "Remove b.example" })).not.toHaveAccessibleDescription();
  });
});
