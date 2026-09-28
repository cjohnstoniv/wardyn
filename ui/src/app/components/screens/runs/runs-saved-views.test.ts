/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, beforeEach } from "vitest";
import {
  BUILTIN_RUNS_VIEWS,
  MAX_SAVED_RUNS_VIEWS,
  loadSavedRunsViews,
  saveRunsView,
} from "./runs-saved-views";

const STORAGE_KEY = "wardyn-runs-saved-views";

beforeEach(() => {
  localStorage.clear();
});

describe("BUILTIN_RUNS_VIEWS — design.md §2.1's own built-in names", () => {
  it("ships exactly the four named views, in order", () => {
    expect(BUILTIN_RUNS_VIEWS.map((v) => v.name)).toEqual([
      "Default",
      "Failed this week",
      "Killed",
      "By workspace",
    ]);
  });

  it("Default is the plain default state — an empty query string", () => {
    expect(BUILTIN_RUNS_VIEWS[0].search).toBe("");
  });

  it("each view is a distinct, non-empty query string past Default", () => {
    const rest = BUILTIN_RUNS_VIEWS.slice(1);
    expect(new Set(rest.map((v) => v.search)).size).toBe(rest.length);
    for (const v of rest) expect(v.search.length).toBeGreaterThan(0);
  });
});

describe("loadSavedRunsViews — H-8", () => {
  it("an empty browser has no saved views", () => {
    expect(loadSavedRunsViews()).toEqual([]);
  });

  it("a corrupted localStorage entry (not JSON) is dropped, not thrown", () => {
    localStorage.setItem(STORAGE_KEY, "{not json");
    expect(loadSavedRunsViews()).toEqual([]);
  });

  it("a corrupted entry that IS json but not an array is dropped", () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ name: "not an array" }));
    expect(loadSavedRunsViews()).toEqual([]);
  });

  it("a malformed row inside an otherwise-valid array is filtered out; valid rows survive", () => {
    localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify([{ name: "ok", search: "status=failed" }, { bogus: true }, { name: 5, search: "x" }, null]),
    );
    expect(loadSavedRunsViews()).toEqual([{ name: "ok", search: "status=failed" }]);
  });
});

describe("saveRunsView — 'Save view' names the current URL (H-8)", () => {
  it("appends a new saved view and persists it", () => {
    const result = saveRunsView("My failures", "status=failed&ended_within=30d");
    expect(result).toEqual([{ name: "My failures", search: "status=failed&ended_within=30d" }]);
    expect(loadSavedRunsViews()).toEqual(result);
  });

  it("a saved view survives being re-read (simulates a reload)", () => {
    saveRunsView("Reload me", "group=title");
    // A fresh read — nothing here reuses the in-memory result above — is the
    // same thing a page reload does: re-run loadSavedRunsViews() from scratch.
    expect(loadSavedRunsViews()).toEqual([{ name: "Reload me", search: "group=title" }]);
  });

  it("caps at MAX_SAVED_RUNS_VIEWS, dropping the oldest first (a rolling window, not a hard refusal)", () => {
    for (let i = 0; i < MAX_SAVED_RUNS_VIEWS + 3; i++) {
      saveRunsView(`view-${i}`, `q=${i}`);
    }
    const saved = loadSavedRunsViews();
    expect(saved.length).toBe(MAX_SAVED_RUNS_VIEWS);
    expect(saved[0].name).toBe("view-3");
    expect(saved[saved.length - 1].name).toBe(`view-${MAX_SAVED_RUNS_VIEWS + 2}`);
  });
});
