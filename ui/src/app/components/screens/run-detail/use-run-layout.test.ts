/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A Service panel the person removed stays removed; a saved layout that predates the panel still gets it. The Desktop
// tile of a VM run is the same: a saved layout from before the tile gets it, and one saved with it taken out keeps it out.
import { beforeEach, describe, it, expect, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { RunLayoutWidget } from "../../../lib/api/run-layout";
import { useRunLayout } from "./use-run-layout";
import { DESKTOP_REMOVED, SERVICE_REMOVED } from "./widget-registry";

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

async function loadedAs(saved: RunLayoutWidget[], preset: "live" | "finished", vm: boolean) {
  getLayout.mockResolvedValue({ preset, layout: saved });
  const hook = renderHook(() => useRunLayout(preset, false, vm));
  await waitFor(() => expect(hook.result.current.loaded).toBe(true));
  return hook;
}

const overlaps = (a: RunLayoutWidget, b: RunLayoutWidget) => a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;
const noOverlap = (l: RunLayoutWidget[]) => l.every((a, i) => l.slice(i + 1).every((b) => !overlaps(a, b)));

describe("useRunLayout: the Desktop tile on a VM run", () => {
  const LIVE_BEFORE: RunLayoutWidget[] = [
    { widget: "terminal", x: 0, y: 0, w: 8, h: 12 },
    { widget: "identity", x: 8, y: 16, w: 4, h: 4 },
    { widget: "ssh", x: 8, y: 20, w: 4, h: 5 },
    { widget: "effective-policy", x: 8, y: 25, w: 4, h: 4 },
  ];
  const FINISHED_BEFORE: RunLayoutWidget[] = [
    { widget: "terminal", x: 0, y: 0, w: 7, h: 8 },
    { widget: "identity", x: 6, y: 8, w: 6, h: 4 },
    { widget: "ssh", x: 6, y: 12, w: 6, h: 5 },
  ];

  it("adds the tile to a saved live layout that predates it, in the Attach tile's place, with nothing overlapping", async () => {
    const { result } = await loadedAs(LIVE_BEFORE, "live", true);
    const l = result.current.layout;
    expect(l.find((w) => w.widget === "desktop")).toEqual({ widget: "desktop", x: 8, y: 20, w: 4, h: 4 });
    expect(l.find((w) => w.widget === "ssh")).toMatchObject({ y: 24 });
    expect(l.find((w) => w.widget === "effective-policy")).toMatchObject({ y: 29 });
    expect(l.find((w) => w.widget === "identity")).toMatchObject({ y: 16 });
    expect(noOverlap(l)).toBe(true);
  });

  it("adds it to a saved finished layout, where the disk line was", async () => {
    const { result } = await loadedAs(FINISHED_BEFORE, "finished", true);
    const l = result.current.layout;
    expect(l.find((w) => w.widget === "desktop")).toMatchObject({ x: 6, y: 12, w: 6, h: 4 });
    expect(noOverlap(l)).toBe(true);
  });

  it("overlaps nothing when a widget in partly the same columns sits below Attach (ssh x8 y24 w4 h2, A x6 y26 w4 h4, C x0 y30 w8 h4)", async () => {
    const saved: RunLayoutWidget[] = [
      { widget: "terminal", x: 0, y: 0, w: 8, h: 12 },
      { widget: "ssh", x: 8, y: 24, w: 4, h: 2 },
      { widget: "identity", x: 6, y: 26, w: 4, h: 4 },
      { widget: "credentials", x: 0, y: 30, w: 8, h: 4 },
    ];
    expect(noOverlap(saved)).toBe(true);
    const { result } = await loadedAs(saved, "live", true);
    const l = result.current.layout;
    expect(l.find((w) => w.widget === "desktop")).toEqual({ widget: "desktop", x: 8, y: 24, w: 4, h: 4 });
    expect(l.find((w) => w.widget === "identity")?.y).toBe(30);
    expect(l.find((w) => w.widget === "credentials")?.y).toBe(34);
    expect(noOverlap(l)).toBe(true);
  });

  it("goes below everything when the saved layout has no Attach tile", async () => {
    const { result } = await loadedAs(SAVED, "live", true);
    expect(result.current.layout.find((w) => w.widget === "desktop")).toEqual({ widget: "desktop", x: 0, y: 12, w: 4, h: 4 });
  });

  it("adds nothing on a run with no desktop, and writes nothing on load", async () => {
    const { result } = await loadedAs(LIVE_BEFORE, "live", false);
    expect(ids(result.current.layout)).not.toContain("desktop");
    expect(putLayout).not.toHaveBeenCalled();
  });

  it("does not add it back to a layout saved with it removed, and keeps the marker out of the layout", async () => {
    const { result } = await loadedAs([...LIVE_BEFORE, DESKTOP_REMOVED], "live", true);
    expect(ids(result.current.layout)).not.toContain("desktop");
    expect(result.current.layout).not.toContainEqual(DESKTOP_REMOVED);
  });

  it("a layout holding only the marker shows the preset without the tile", async () => {
    const { result } = await loadedAs([DESKTOP_REMOVED], "live", true);
    expect(ids(result.current.layout)).toContain("terminal");
    expect(ids(result.current.layout)).not.toContain("desktop");
  });

  it("saves a removal as the marker, and the next load keeps the tile out", async () => {
    const first = await loadedAs(LIVE_BEFORE, "live", true);
    act(() => first.result.current.apply(first.result.current.layout.filter((w) => w.widget !== "desktop")));
    await act(async () => void (await first.result.current.save()));
    const body = putLayout.mock.calls[0][1] as RunLayoutWidget[];
    expect(body.filter((w) => w.widget === "desktop")).toEqual([DESKTOP_REMOVED]);
    const again = await loadedAs(body, "live", true);
    expect(ids(again.result.current.layout)).not.toContain("desktop");
  });

  it("drops the marker when the person adds the tile back", async () => {
    const { result } = await loadedAs([...LIVE_BEFORE, DESKTOP_REMOVED], "live", true);
    act(() => result.current.apply([...result.current.layout, { widget: "desktop", x: 0, y: 30, w: 4, h: 4 }]));
    await act(async () => void (await result.current.save()));
    const body = putLayout.mock.calls[0][1] as RunLayoutWidget[];
    expect(body).not.toContainEqual(DESKTOP_REMOVED);
    expect(ids(body)).toContain("desktop");
  });

  it("keeps the marker through an edit made on a run with no desktop", async () => {
    const { result } = await loadedAs([...LIVE_BEFORE, DESKTOP_REMOVED], "live", false);
    act(() => result.current.apply(result.current.layout.filter((w) => w.widget !== "egress")));
    await act(async () => void (await result.current.save()));
    expect(putLayout.mock.calls[0][1]).toContainEqual(DESKTOP_REMOVED);
  });

  it("reset clears the marker and brings the tile back", async () => {
    const { result } = await loadedAs([...LIVE_BEFORE, DESKTOP_REMOVED], "live", true);
    act(() => result.current.reset());
    expect(ids(result.current.layout)).toContain("desktop");
    expect(putLayout.mock.calls[0][1]).toEqual([]);
  });
});

describe("useRunLayout: the default rail by kind of run", () => {
  const y = (l: RunLayoutWidget[], id: string) => l.find((w) => w.widget === id)?.y;

  it("a VM run with nothing saved has the Desktop tile, Attach under it and Effective policy under that", async () => {
    const { result } = await loadedAs([], "live", true);
    expect([y(result.current.layout, "desktop"), y(result.current.layout, "ssh"), y(result.current.layout, "effective-policy")]).toEqual([24, 28, 33]);
  });

  it("any other run with nothing saved has no Desktop tile and no gap above Effective policy", async () => {
    const { result } = await loadedAs([], "live", false);
    expect(ids(result.current.layout)).not.toContain("desktop");
    expect([y(result.current.layout, "ssh"), y(result.current.layout, "effective-policy")]).toEqual([24, 29]);
  });

  it("the first paint, before the saved layout arrives, is the same", () => {
    getLayout.mockReturnValue(new Promise(() => {}));
    expect(y(renderHook(() => useRunLayout("live", false, false)).result.current.layout, "effective-policy")).toBe(29);
    expect(y(renderHook(() => useRunLayout("live", false, true)).result.current.layout, "effective-policy")).toBe(33);
  });
});

// The write path: the server's upsert is unconditional, so the client keeps one request per preset in flight, and a
// failed write is remembered with its preset and exact body so Retry repeats THAT, whatever is showing by then.
describe("useRunLayout: write ordering and retry", () => {
  const deferred = () => {
    let resolve!: (v: unknown) => void;
    let reject!: (e: unknown) => void;
    const promise = new Promise((res, rej) => {
      resolve = res;
      reject = rej;
    });
    return { promise, resolve, reject };
  };
  const A: RunLayoutWidget[] = [{ widget: "terminal", x: 0, y: 0, w: 12, h: 12 }];

  it("never has two writes for one preset in flight, and the last action is the last body stored", async () => {
    const { result } = await loaded(SAVED, false);
    const first = deferred();
    putLayout.mockReturnValueOnce(first.promise);

    act(() => result.current.apply(A));
    let flushed!: Promise<unknown>;
    act(() => void (flushed = result.current.save())); // the drag's PUT, slow
    act(() => result.current.reset()); // the person resets while it is in flight
    act(() => result.current.apply(SAVED)); // ...and rearranges again
    act(() => void result.current.reset()); // ...and resets: the LAST action
    expect(putLayout).toHaveBeenCalledTimes(1); // nothing overlaps

    await act(async () => {
      first.resolve({ preset: "live", layout: A });
      await flushed;
    });
    await waitFor(() => expect(putLayout).toHaveBeenCalledTimes(2));
    // Only the newest waiting body goes out after the slow one, so the stored row ends as the last action left it.
    expect(putLayout.mock.calls.map((c) => c[1])).toEqual([A, []]);
  });

  it("Retry repeats the failed write for its own preset after the preset has switched", async () => {
    getLayout.mockResolvedValue({ preset: "live", layout: SAVED });
    const hook = renderHook(({ p }: { p: "live" | "finished" }) => useRunLayout(p), { initialProps: { p: "live" } });
    await waitFor(() => expect(hook.result.current.loaded).toBe(true));
    const slow = deferred();
    putLayout.mockReturnValueOnce(slow.promise);

    act(() => hook.result.current.reset()); // Live reset, in flight
    hook.rerender({ p: "finished" }); // the run finished / Finished was chosen
    await waitFor(() => expect(hook.result.current.loaded).toBe(true));
    await act(async () => slow.reject(new Error("network")));

    expect(hook.result.current.status).toBe("failed");
    expect(hook.result.current.failedPreset).toBe("live");

    putLayout.mockResolvedValue({ preset: "live", layout: [] });
    act(() => hook.result.current.retry());
    await waitFor(() => expect(putLayout).toHaveBeenCalledTimes(2));
    expect(putLayout.mock.calls[1]).toEqual(["live", []]);
    await waitFor(() => expect(hook.result.current.status).not.toBe("failed"));
  });
});
