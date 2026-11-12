/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Machine tile of a run that keeps a machine: a saved layout from before the tile gets it, and one saved with it taken
// out keeps it out, as the Desktop tile does.
import { beforeEach, describe, it, expect, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { RunLayoutWidget } from "../../../lib/api/run-layout";
import { useRunLayout } from "./use-run-layout";
import { MACHINE_REMOVED } from "./widget-registry";

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
  getLayout.mockReset();
  putLayout.mockReset();
  putLayout.mockResolvedValue({ preset: "live", layout: [] });
});

async function loadedAs(saved: RunLayoutWidget[], machine: boolean) {
  getLayout.mockResolvedValue({ preset: "live", layout: saved });
  const hook = renderHook(() => useRunLayout("live", false, false, machine));
  await waitFor(() => expect(hook.result.current.loaded).toBe(true));
  return hook;
}

describe("useRunLayout: the Machine tile on a run with a kept machine", () => {
  it("adds the tile to a saved layout that predates it, and nothing on a run with no machine", async () => {
    expect(ids((await loadedAs(SAVED, true)).result.current.layout)).toContain("machine");
    expect(ids((await loadedAs(SAVED, false)).result.current.layout)).not.toContain("machine");
    expect(putLayout).not.toHaveBeenCalled();
  });

  it("does not add it back to a layout saved with it removed, and keeps the marker out of the layout", async () => {
    const { result } = await loadedAs([...SAVED, MACHINE_REMOVED], true);
    expect(ids(result.current.layout)).toEqual(["terminal", "egress"]);
  });

  it("saves a removal as the marker, and the next load keeps the tile out", async () => {
    const first = await loadedAs(SAVED, true);
    act(() => first.result.current.apply(first.result.current.layout.filter((w) => w.widget !== "machine")));
    await act(async () => void (await first.result.current.save()));
    const body = putLayout.mock.calls[0][1] as RunLayoutWidget[];
    expect(body.filter((w) => w.widget === "machine")).toEqual([MACHINE_REMOVED]);
    expect(ids((await loadedAs(body, true)).result.current.layout)).not.toContain("machine");
  });

  it("drops the marker when the person adds the tile back, and keeps it through an edit on a run with no machine", async () => {
    const back = await loadedAs([...SAVED, MACHINE_REMOVED], true);
    act(() => back.result.current.apply([...back.result.current.layout, { widget: "machine", x: 8, y: 4, w: 4, h: 4 }]));
    await act(async () => void (await back.result.current.save()));
    expect(putLayout.mock.calls[0][1]).not.toContainEqual(MACHINE_REMOVED);
    putLayout.mockClear();
    const plain = await loadedAs([...SAVED, MACHINE_REMOVED], false);
    act(() => plain.result.current.apply(plain.result.current.layout.filter((w) => w.widget !== "egress")));
    await act(async () => void (await plain.result.current.save()));
    expect(putLayout.mock.calls[0][1]).toContainEqual(MACHINE_REMOVED);
  });

  it("reset brings the tile back", async () => {
    const { result } = await loadedAs([...SAVED, MACHINE_REMOVED], true);
    act(() => result.current.reset());
    expect(ids(result.current.layout)).toContain("machine");
    expect(putLayout.mock.calls[0][1]).toEqual([]);
  });
});
