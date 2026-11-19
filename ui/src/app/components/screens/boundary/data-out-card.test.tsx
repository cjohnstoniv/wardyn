/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The boundary's Data out card: one row of three values under a header with the value at a glance, the lock
// that adds "data_out" to the boundary's locked knobs, and the footer naming the doors. The sentences are
// written out here, so a drift in copy/data-out.ts fails this suite.
import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DataOutCard } from "./data-out-card";

type Value = "allowed" | "review" | "blocked" | undefined;

function renderCard(value: Value, locks: string[] = [], over: { disabled?: boolean } = {}) {
  const onChange = vi.fn();
  const onLocksChange = vi.fn();
  render(<DataOutCard value={value} locks={locks} disabled={over.disabled ?? false} onChange={onChange} onLocksChange={onLocksChange} />);
  return { onChange, onLocksChange };
}

const row = () => screen.getByRole("group", { name: "Content leaving a run" });
const checked = () => within(row()).getByRole("radio", { checked: true }).getAttribute("value");

describe("DataOutCard", () => {
  it("shows the count, the applies line, the Now line, three values, the below line and the footer", () => {
    renderCard("review");
    const card = screen.getByTestId("boundary-data-out");
    expect(within(card).getByRole("heading", { name: "Data out" })).toBeInTheDocument();
    expect(within(card).getByText("1 setting")).toBeInTheDocument();
    expect(within(card).getByText("Applies to every run, break-glass included.")).toBeInTheDocument();
    expect(screen.getByTestId("boundary-data-out-now")).toHaveTextContent("NowContent leaving a runReview");
    expect(within(row()).getAllByRole("radio").map((r) => r.getAttribute("value"))).toEqual(["allowed", "review", "blocked"]);
    expect(checked()).toBe("review");
    expect(within(card).getByText("Profiles below may only hold in more.")).toBeInTheDocument();
    expect(
      within(card).getByText(
        "Enforced by Wardyn at each door: the Files panel, SSH, run output, recording replay, web apps and computer folders. Reading the terminal or the screen is not covered: turn off interactive access for that.",
      ),
    ).toBeInTheDocument();
  });

  it("a boundary that names no rule reads as Allowed", () => {
    renderCard(undefined);
    expect(checked()).toBe("allowed");
    expect(screen.getByTestId("boundary-data-out-now")).toHaveTextContent("NowContent leaving a runAllowed");
  });

  it("picking a value reports it", async () => {
    const { onChange } = renderCard(undefined);
    await userEvent.click(within(row()).getByRole("radio", { name: "Blocked" }));
    expect(onChange).toHaveBeenCalledWith("blocked");
  });

  it("the lock adds and removes data_out, keeping the other locks", async () => {
    const a = renderCard("blocked", ["allowed_domains"]);
    await userEvent.click(within(row()).getByRole("checkbox", { name: "Lock for profiles below" }));
    expect(a.onLocksChange).toHaveBeenCalledWith(["allowed_domains", "data_out"]);
    document.body.innerHTML = "";
    const b = renderCard("blocked", ["allowed_domains", "data_out"]);
    const box = within(row()).getByRole("checkbox", { name: "Lock for profiles below" });
    expect(box).toBeChecked();
    await userEvent.click(box);
    expect(b.onLocksChange).toHaveBeenCalledWith(["allowed_domains"]);
  });

  it("a read-only page leaves the row's controls off", () => {
    renderCard("review", [], { disabled: true });
    for (const r of within(row()).getAllByRole("radio")) expect(r).toBeDisabled();
  });
});
