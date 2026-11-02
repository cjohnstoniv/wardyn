/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A Service panel the person removed stays removed; a saved layout that predates the panel still gets it.
import { beforeEach, describe, it, expect, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { RunLayoutWidget } from "../../../lib/api/run-layout";
import { useRunLayout } from "./use-run-layout";
import { SERVICE_REMOVED } from "./widget-registry";

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

async function loaded(saved: RunLayoutWidget[], service: boolean) {
  getLayout.mockResolvedValue({ preset: "live", layout: saved });
  const hook = renderHook(() => useRunLayout("live", service));
  await waitFor(() => expect(hook.result.current.loaded).toBe(true));
  return hook;
}

describe("useRunLayout: the Service panel on a service run", () => {
  it("adds the panel to a saved layout that predates it", async () => {
    const { result } = await loaded(SAVED, true);
    expect(ids(result.current.layout)).toContain("service");
  });

  it("does not add it back to a layout saved with it removed, and keeps the marker out of the layout", async () => {
    const { result } = await loaded([...SAVED, SERVICE_REMOVED], true);
    expect(ids(result.current.layout)).toEqual(["terminal", "egress"]);
  });

  it("saves a removal as the marker, and the next load keeps the panel out", async () => {
    const first = await loaded(SAVED, true);
    act(() => first.result.current.apply(first.result.current.layout.filter((w) => w.widget !== "service")));
    await act(async () => void (await first.result.current.save()));
    const body = putLayout.mock.calls[0][1] as RunLayoutWidget[];
    expect(body).toContainEqual(SERVICE_REMOVED);
    expect(body.filter((w) => w.widget === "service")).toEqual([SERVICE_REMOVED]);

    const again = await loaded(body, true);
    expect(ids(again.result.current.layout)).not.toContain("service");
  });

  it("drops the marker when the person adds the panel back", async () => {
    const { result } = await loaded([...SAVED, SERVICE_REMOVED], true);
    act(() => result.current.apply([...result.current.layout, { widget: "service", x: 8, y: 0, w: 4, h: 5 }]));
    await act(async () => void (await result.current.save()));
    const body = putLayout.mock.calls[0][1] as RunLayoutWidget[];
    expect(body).not.toContainEqual(SERVICE_REMOVED);
    expect(ids(body)).toContain("service");
  });

  it("keeps the marker through an edit made on a run with no panel", async () => {
    const { result } = await loaded([...SAVED, SERVICE_REMOVED], false);
    act(() => result.current.apply(result.current.layout.map((w) => (w.widget === "egress" ? { ...w, h: 6 } : w))));
    await act(async () => void (await result.current.save()));
    expect(putLayout.mock.calls[0][1]).toContainEqual(SERVICE_REMOVED);
  });

  it("writes no marker for an edit on a run with no panel when the person never removed it", async () => {
    const { result } = await loaded(SAVED, false);
    act(() => result.current.apply(result.current.layout.map((w) => (w.widget === "egress" ? { ...w, h: 6 } : w))));
    await act(async () => void (await result.current.save()));
    expect(putLayout.mock.calls[0][1]).not.toContainEqual(SERVICE_REMOVED);
  });

  it("Reset clears the marker with the rest of the saved layout", async () => {
    const { result } = await loaded([...SAVED, SERVICE_REMOVED], true);
    act(() => result.current.reset());
    expect(putLayout).toHaveBeenCalledWith("live", []);
    expect(ids(result.current.layout)).toContain("service");
  });
});
