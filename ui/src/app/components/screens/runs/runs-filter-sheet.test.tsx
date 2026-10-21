/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #3041 — the phone filter bar: the Filters (n) button, its sheet, and the
// removable chips. jsdom loads no CSS, so the inline controls and the chips
// are both in the DOM here; the 390 px layout itself is pinned by
// ui/e2e/runs-filters-phone.spec.ts.
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { RunsFilterBar } from "./runs-filter-bar";
import { DEFAULT_RUNS_FILTERS, type RunsFilterState } from "./runs-filters";

function bar(over: Partial<RunsFilterState>, runCount = 9, runsTruncated = false) {
  const onChange = vi.fn();
  render(
    <RunsFilterBar
      filters={{ ...DEFAULT_RUNS_FILTERS, ...over }}
      workspaces={["acme/widgets"]}
      adminView={false}
      currentSearch=""
      savedViews={[]}
      runCount={runCount}
      runsTruncated={runsTruncated}
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

describe("RunsFilterBar phone sheet across the sm breakpoint", () => {
  afterEach(() => {
    delete (window as { matchMedia?: unknown }).matchMedia;
  });

  it("an open sheet closes when the viewport grows to sm, so the page is not left dimmed and locked", async () => {
    let listener: (() => void) | undefined;
    const mq = {
      matches: false,
      addEventListener: (_: string, l: () => void) => (listener = l),
      removeEventListener: () => (listener = undefined),
    };
    window.matchMedia = vi.fn(() => mq) as unknown as typeof window.matchMedia;
    bar({ status: "needs" });
    fireEvent.click(screen.getByRole("button", { name: "Filters (1)" }));
    expect(screen.getByRole("dialog", { name: "Filters" })).toBeInTheDocument();
    expect(window.matchMedia).toHaveBeenCalledWith("(min-width: 640px)");
    // Still a phone: a change event that does not cross sm leaves it open.
    act(() => listener?.());
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    mq.matches = true;
    act(() => listener?.());
    expect(screen.queryByRole("dialog")).toBeNull();
    // The Filters button is hidden at sm and up, so focus goes to the search box, not the body.
    await waitFor(() => expect(screen.getByLabelText("Search runs")).toHaveFocus());
  });

  it("Show N runs: one run reads the one form, and a list cut at its limit reads N+", () => {
    bar({ status: "needs" }, 1);
    fireEvent.click(screen.getByRole("button", { name: "Filters (1)" }));
    expect(screen.getByRole("button", { name: "Show 1 run" })).toBeInTheDocument();
  });

  it("Show N runs reads N+ when the list is cut at its limit, as the page header does", () => {
    bar({ status: "needs" }, 200, true);
    fireEvent.click(screen.getByRole("button", { name: "Filters (1)" }));
    expect(screen.getByRole("button", { name: "Show 200+ runs" })).toBeInTheDocument();
  });
});
