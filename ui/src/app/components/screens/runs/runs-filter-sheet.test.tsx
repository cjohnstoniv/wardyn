/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #3041 — the phone filter bar: the Filters (n) button, its sheet, and the
// removable chips. jsdom loads no CSS, so the inline controls and the chips
// are both in the DOM here; the 390 px layout itself is pinned by
// ui/e2e/runs-filters-phone.spec.ts.
import { describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";

import { RunsFilterBar } from "./runs-filter-bar";
import { DEFAULT_RUNS_FILTERS, type RunsFilterState } from "./runs-filters";

function bar(over: Partial<RunsFilterState>, runCount = 9) {
  const onChange = vi.fn();
  render(
    <RunsFilterBar
      filters={{ ...DEFAULT_RUNS_FILTERS, ...over }}
      workspaces={["acme/widgets"]}
      adminView={false}
      currentSearch=""
      savedViews={[]}
      runCount={runCount}
      onChange={onChange}
      onSelectView={vi.fn()}
      onSaveView={vi.fn()}
    />,
  );
  return onChange;
}

describe("RunsFilterBar phone filters", () => {
  it("the button counts the controls off their default; search is not one", () => {
    bar({ q: "deploy", status: "needs", group: "workspace" });
    expect(screen.getByRole("button", { name: "Filters (2)" })).toBeInTheDocument();
  });

  it("reads plain Filters when nothing is active, and shows no chips", () => {
    bar({});
    expect(screen.getByRole("button", { name: "Filters" })).toBeInTheDocument();
    expect(screen.queryByTestId("runs-filter-chips")).toBeNull();
  });

  it("removing a chip resets only that control", () => {
    const onChange = bar({ q: "deploy", status: "needs", group: "workspace" });
    const chips = within(screen.getByTestId("runs-filter-chips"));
    fireEvent.click(chips.getByRole("button", { name: "Remove Status · Needs you" }));
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith({ ...DEFAULT_RUNS_FILTERS, q: "deploy", group: "workspace" });
  });

  it("the sheet holds Save view and a Show N runs button; Clear filters keeps the search", () => {
    const onChange = bar({ q: "deploy", status: "needs" }, 9);
    fireEvent.click(screen.getByRole("button", { name: "Filters (1)" }));
    const sheet = within(screen.getByRole("dialog", { name: "Filters" }));
    expect(sheet.getByRole("button", { name: "Save view" })).toBeInTheDocument();
    expect(sheet.getByRole("button", { name: "Show 9 runs" })).toBeInTheDocument();
    fireEvent.click(sheet.getByRole("button", { name: "Clear filters" }));
    expect(onChange).toHaveBeenCalledWith({ ...DEFAULT_RUNS_FILTERS, q: "deploy" });
    fireEvent.click(sheet.getByRole("button", { name: "Show 9 runs" }));
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
