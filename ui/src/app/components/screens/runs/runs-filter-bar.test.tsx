/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1727 — the runs search box. The URL answers a keystroke a render late, so
// the box holds a local draft; these pin the three cases that decide whether
// that draft is right: typing ahead of the URL, an outside q, and a type then
// revert, where the URL never moves and a stale pending entry would otherwise
// swallow the next saved view.
//
// Each keystroke is its own setField call, the repo's one-change-event form of
// typing: the box must keep every intermediate value, which is what a single
// setField to the final string would not exercise.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";

import { RunsFilterBar } from "./runs-filter-bar";
import { DEFAULT_RUNS_FILTERS, type RunsFilterState } from "./runs-filters";
import { setField } from "../../../../test/set-field";

function filters(q: string): RunsFilterState {
  return { ...DEFAULT_RUNS_FILTERS, q };
}

function bar(q: string, onChange = vi.fn()) {
  const view = render(
    <RunsFilterBar
      filters={filters(q)}
      workspaces={[]}
      adminView={false}
      currentSearch=""
      savedViews={[]}
      onChange={onChange}
      onSelectView={vi.fn()}
      onSaveView={vi.fn()}
    />,
  );
  return { ...view, onChange };
}

const box = () => screen.getByLabelText("Search runs");

function showQ(view: ReturnType<typeof bar>, q: string): void {
  view.rerender(
    <RunsFilterBar
      filters={filters(q)}
      workspaces={[]}
      adminView={false}
      currentSearch=""
      savedViews={[]}
      onChange={view.onChange}
      onSelectView={vi.fn()}
      onSaveView={vi.fn()}
    />,
  );
}

describe("RunsFilterBar search box", () => {
  it("keeps every character typed while filters.q still lags the URL", () => {
    // The parent never re-renders, so filters.q stays "" for every keystroke.
    const view = bar("");
    setField(box(), "a");
    setField(box(), "ab");
    setField(box(), "abc");
    expect(box()).toHaveValue("abc");
    // One change per keystroke, exactly as before the draft — no extra fetch.
    expect(view.onChange.mock.calls.map(([f]) => f.q)).toEqual(["a", "ab", "abc"]);
  });

  it("takes an outside q as the answer when it is not one of ours", () => {
    const view = bar("");
    setField(box(), "abc");
    // A saved view picked elsewhere: the draft follows it.
    showQ(view, "deploy");
    expect(box()).toHaveValue("deploy");
  });

  it("clears the queue on a type-then-revert, so a later q is not swallowed", () => {
    const view = bar("");
    setField(box(), "a");
    setField(box(), ""); // the backspace
    expect(box()).toHaveValue("");
    // filters.q never moved (both keystrokes were to the URL's own ""), so the
    // "a" and "" this box sent are still queued. A saved view that searches
    // for "a" must still replace the draft.
    showQ(view, "a");
    expect(box()).toHaveValue("a");
  });
});