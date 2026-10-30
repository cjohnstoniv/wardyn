/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Desktop group card (board M7-Knobs-B, #2967): the board's words, the value each direction is held to
// (the stricter one where the boundary leaves a direction at its default), and a change names all four. Each
// direction locks on its own (desktop.<direction>), and the group lock sets or clears all four.
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BOUNDARY_DESKTOP as C } from "../../../lib/boundary-copy";
import type { DesktopControls } from "../../../lib/types/policy";
import { DesktopGroupCard } from "./desktop-group-card";

const checked = (row: string) => {
  const group = screen.getByRole("group", { name: row });
  return within(group).getByRole("radio", { checked: true }).getAttribute("value");
};

function renderCard(value: DesktopControls | undefined, onChange = vi.fn(), locks: string[] = [], onLocksChange = vi.fn()) {
  render(<DesktopGroupCard value={value} locks={locks} disabled={false} onChange={onChange} onLocksChange={onLocksChange} />);
  return onChange;
}

const ALL_LOCKS = ["desktop.copy_out", "desktop.download", "desktop.paste_in", "desktop.upload"];
const rowLock = (row: string) => within(screen.getByRole("group", { name: row })).getByRole("checkbox", { name: C.LOCK_ROW });
const groupLock = () => screen.getByRole("checkbox", { name: C.LOCK_GROUP });

describe("DesktopGroupCard", () => {
  it("shows the board's words", () => {
    renderCard(undefined);
    const card = screen.getByTestId("boundary-desktop");
    for (const s of [C.TITLE, C.COUNT(4), C.APPLIES, C.NOW, C.BELOW, C.ENFORCED, C.LOCK_GROUP, C.LOCK_ROW]) {
      expect(within(card).getAllByText(s).length).toBeGreaterThan(0);
    }
    for (const r of C.ROWS) {
      expect(within(card).getByRole("group", { name: r.label })).toBeInTheDocument();
    }
    expect(C.COUNT(4)).toBe("4 settings");
  });

  it("a boundary that names no desktop bounds no direction", () => {
    renderCard(undefined);
    for (const r of C.ROWS) expect(checked(r.label)).toBe("allowed");
  });

  it("renders the stricter wall value: a direction the boundary leaves unset is held to its default", () => {
    renderCard({ paste_in: "allowed", copy_out: "allowed", upload: "blocked" });
    expect(checked("Paste into the desktop")).toBe("allowed");
    expect(checked("Copy out of the desktop")).toBe("allowed");
    expect(checked("Upload files")).toBe("blocked");
    expect(checked("Download files")).toBe("blocked");
    const now = screen.getByTestId("boundary-desktop-now");
    for (const s of ["Paste in allowed", "Copy out allowed", "Upload blocked", "Download blocked"]) {
      expect(within(now).getByText((_, el) => el?.tagName === "SPAN" && el.textContent === s)).toBeInTheDocument();
    }
  });

  it("blocking one direction sends all four, the others as shown", async () => {
    const onChange = renderCard(undefined);
    const group = screen.getByRole("group", { name: "Copy out of the desktop" });
    await userEvent.setup().click(within(group).getByRole("radio", { name: C.BLOCKED }));
    expect(onChange).toHaveBeenCalledWith({ paste_in: "allowed", copy_out: "blocked", upload: "allowed", download: "allowed" });
  });

  it("a direction's lock adds only that direction's knob, and the stored locks are kept", async () => {
    const onLocks = vi.fn();
    renderCard(undefined, vi.fn(), ["allowed_domains"], onLocks);
    expect(rowLock("Copy out of the desktop")).not.toBeChecked();
    await userEvent.setup().click(rowLock("Copy out of the desktop"));
    expect(onLocks).toHaveBeenCalledWith(["allowed_domains", "desktop.copy_out"]);
  });

  it("the group lock locks all four directions, and clears all four", async () => {
    const onLocks = vi.fn();
    renderCard(undefined, vi.fn(), ["desktop.download"], onLocks);
    expect(groupLock()).not.toBeChecked();
    await userEvent.setup().click(groupLock());
    expect(onLocks).toHaveBeenLastCalledWith(ALL_LOCKS);
  });

  it("with all four locked the group lock is on, and clearing it unlocks every direction", async () => {
    const onLocks = vi.fn();
    renderCard(undefined, vi.fn(), ["denied_domains", ...ALL_LOCKS], onLocks);
    expect(groupLock()).toBeChecked();
    for (const r of C.ROWS) expect(rowLock(r.label)).toBeChecked();
    await userEvent.setup().click(groupLock());
    expect(onLocks).toHaveBeenLastCalledWith(["denied_domains"]);
  });

  it("a whole-desktop lock shows every direction locked; clearing one keeps the other three", async () => {
    const onLocks = vi.fn();
    renderCard(undefined, vi.fn(), ["desktop"], onLocks);
    expect(groupLock()).toBeChecked();
    await userEvent.setup().click(rowLock("Copy out of the desktop"));
    expect(onLocks).toHaveBeenLastCalledWith(["desktop.download", "desktop.paste_in", "desktop.upload"]);
  });
});
