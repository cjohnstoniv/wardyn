/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Preview URLs tile in a saved layout: a live layout from before the tile gets it, one saved with it taken out keeps it
// out, and taking it out is written as the marker. The finished preset never gets it.
import { beforeEach, describe, it, expect, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { RunLayoutWidget } from "../../../lib/api/run-layout";
import type { RunLayoutPreset } from "../../../lib/api/run-layout";
import { useRunLayout } from "./use-run-layout";
import { PREVIEWS_REMOVED } from "./widget-registry";

const getLayout = vi.fn();
const putLayout = vi.fn();
vi.mock("../../../lib/api/run-layout", () => ({
  runLayout: { getLayout: (...a: unknown[]) => getLayout(...a), putLayout: (...a: unknown[]) => putLayout(...a) },
}));

const SAVED: RunLayoutWidget[] = [
  { widget: "terminal", x: 0, y: 0, w: 8, h: 12 },
  { widget: "egress", x: 8, y: 0, w: 4, h: 4 },
];
const ids = (l: RunLayoutWidget[]) => l.map((w) => w.widget);

beforeEach(() => {
  vi.useRealTimers();
  getLayout.mockReset();
  putLayout.mockReset();
  putLayout.mockResolvedValue({ preset: "live", layout: [] });
});

async function loadedAs(saved: RunLayoutWidget[], preset: RunLayoutPreset = "live") {
  getLayout.mockResolvedValue({ preset, layout: saved });
  const hook = renderHook(() => useRunLayout(preset));
  await waitFor(() => expect(hook.result.current.loaded).toBe(true));
  return hook;
}

describe("useRunLayout: the Preview URLs tile", () => {
  it("adds the tile to a saved live layout that predates it, without writing anything", async () => {
    const { result } = await loadedAs(SAVED);
    expect(result.current.layout.find((w) => w.widget === "previews")).toEqual({ widget: "previews", x: 8, y: 4, w: 4, h: 5 });
    expect(putLayout).not.toHaveBeenCalled();
  });

  it("does not add it to a saved finished layout", async () => {
    expect(ids((await loadedAs(SAVED, "finished")).result.current.layout)).not.toContain("previews");
  });

  it("does not add it back to a live layout saved with it removed, and keeps the marker out of the layout", async () => {
    const { result } = await loadedAs([...SAVED, PREVIEWS_REMOVED]);
    expect(ids(result.current.layout)).toEqual(["terminal", "egress"]);
  });

  it("keeps the preset's own tile for a person who never saved a layout, and drops it when only the marker is saved", async () => {
    expect(ids((await loadedAs([])).result.current.layout)).toContain("previews");
    expect(ids((await loadedAs([PREVIEWS_REMOVED])).result.current.layout)).not.toContain("previews");
  });

  it("writes the marker when the person takes the tile out of a live layout, and not otherwise", async () => {
    vi.useFakeTimers();
    getLayout.mockResolvedValue({ preset: "live", layout: SAVED });
    const { result } = renderHook(() => useRunLayout("live"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    act(() => result.current.apply(result.current.layout.filter((w) => w.widget !== "previews")));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });
    expect(putLayout).toHaveBeenCalledTimes(1);
    const body = putLayout.mock.calls[0][1] as RunLayoutWidget[];
    expect(body).toContainEqual(PREVIEWS_REMOVED);
    expect(ids(body)).not.toContain("machine");
  });
});
