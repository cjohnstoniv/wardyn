/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import { HttpError } from "../../../lib/api/core";
import type { WidgetContext } from "./widget-registry";

const getFiles = vi.fn();
const getResources = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: { getFiles: (...a: unknown[]) => getFiles(...a), getResources: (...a: unknown[]) => getResources(...a), getModelUsage: () => Promise.resolve({ totals: { input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, reasoning_tokens: 0, calls: 0, uncounted_calls: 0, partial_calls: 0 }, by_model: [], opaque_tunnels: 0 }) },
}));

import { RunStatusStrip, useEvidenceFacts } from "./status-strip";

const ctx = (over: Partial<WidgetContext> = {}) =>
  ({
    run: { id: "run-1" },
    finished: false,
    grants: [],
    egress: [],
    audit: [],
    heldCount: 0,
    ...over,
  }) as unknown as WidgetContext;

const tile = (label: string) => within(screen.getByText(label).closest('[data-testid="status-tile"]') as HTMLElement);

beforeEach(() => {
  getFiles.mockReset();
  getResources.mockReset();
});

describe("RunStatusStrip (#3038)", () => {
  it("is four tiles: Egress, Files, Sandbox, Credentials", async () => {
    render(<RunStatusStrip ctx={ctx({ evidence: { files: null, sandbox: null } })} />);
    await screen.findAllByText("Not available");
    const labels = screen.getAllByTestId("status-tile").map((t) => t.firstElementChild?.textContent);
    expect(labels).toEqual(["Egress", "Files", "Sandbox", "Credentials"]);
  });

  it("the Sandbox tile says Not available, and Files says Not inspectable, when the runner offers nothing", async () => {
    render(<RunStatusStrip ctx={ctx({ finished: true, evidence: { files: null, sandbox: null } })} />);
    expect(await tile("Sandbox").findByText("Not available")).toBeInTheDocument();
    expect(tile("Files").getByText("Not inspectable")).toBeInTheDocument();
    expect(screen.queryByText("n/a")).toBeNull();
  });

  it("reads real evidence when there is some", async () => {
    render(<RunStatusStrip ctx={ctx({ evidence: { files: 2, sandbox: "12%" } })} />);
    expect(await tile("Files").findByText("2")).toBeInTheDocument();
    expect(tile("Sandbox").getByText("12%")).toBeInTheDocument();
  });

  it("Egress leads with what is held, Credentials counts grants and mints", async () => {
    render(
      <RunStatusStrip
        ctx={ctx({
          evidence: { files: 0, sandbox: null },
          heldCount: 1,
          grants: [{ id: "g" }] as unknown as WidgetContext["grants"],
          audit: [{ action: "credential.mint", outcome: "success" }] as unknown as WidgetContext["audit"],
        })}
      />,
    );
    expect(tile("Egress").getByText("1 held")).toBeInTheDocument();
    expect(tile("Credentials").getByText("1 eligible · 1 minted")).toBeInTheDocument();
    expect(await tile("Files").findByText("None yet")).toBeInTheDocument();
  });

  it("Egress says None yet with nothing recorded", async () => {
    render(<RunStatusStrip ctx={ctx({ evidence: { files: 0, sandbox: null } })} />);
    expect(tile("Egress").getByText("None yet")).toBeInTheDocument();
    await screen.findAllByText("Not available");
  });
});

describe("RunStatusStrip egress denies (UXR-23)", () => {
  const egress = (allow: number, deny: number) =>
    [...Array(allow).fill("allow"), ...Array(deny).fill("deny")].map((decision) => ({ decision })) as unknown as WidgetContext["egress"];
  const egressText = () => (screen.getAllByTestId("status-tile")[0].lastElementChild as HTMLElement).textContent;

  it("says the denies beside the allows, the deny count in the warning tone", () => {
    render(<RunStatusStrip ctx={ctx({ evidence: { files: 0, sandbox: null }, egress: egress(40, 3) })} />);
    expect(egressText()).toBe("40 allow · 3 deny");
    expect(tile("Egress").getByText("3 deny").className).toMatch(/text-warning/);
    expect(tile("Egress").getByText("40 allow").className).not.toMatch(/text-warning/);
  });

  it("keeps Held first", () => {
    render(<RunStatusStrip ctx={ctx({ evidence: { files: 0, sandbox: null }, heldCount: 2, egress: egress(40, 3) })} />);
    expect(egressText()).toBe("2 held · 40 allow · 3 deny");
  });
});

describe("RunStatusStrip: the four-column grid and the cut-off values", () => {
  it("goes to four columns only at xl, where the sidebar leaves room, not at lg", () => {
    render(<RunStatusStrip ctx={ctx({ evidence: { files: 0, sandbox: null } })} />);
    const grid = screen.getByRole("group");
    expect(grid.className).toContain("xl:grid-cols-4");
    expect(grid.className).not.toContain("lg:grid-cols-4");
  });

  it("carries each value in full as a tooltip, since the tile truncates it", () => {
    render(
      <RunStatusStrip
        ctx={ctx({
          evidence: { files: 0, sandbox: null },
          heldCount: 2,
          egress: [...Array(40).fill("allow"), ...Array(3).fill("deny")].map((decision) => ({ decision })) as unknown as WidgetContext["egress"],
          grants: [{ id: "g" }] as unknown as WidgetContext["grants"],
        })}
      />,
    );
    expect(tile("Egress").getByText("3 deny").closest("[title]")).toHaveAttribute("title", "2 held · 40 allow · 3 deny");
    expect(tile("Credentials").getByText("1 eligible · 0 minted")).toHaveAttribute("title", "1 eligible · 0 minted");
  
});
});

describe("useEvidenceFacts (#3038)", () => {
  // The tile has no room for the card's line saying the number is the guest's own (#3924).
  it("does not read a macOS VM guest's CPU for the tile, and still reads its files", async () => {
    getFiles.mockResolvedValue({ vcs: "git", files: [{ path: "a" }] });
    getResources.mockResolvedValue({ cpu_percent: 12.4 });
    const mac = renderHook(() => useEvidenceFacts("run-1", false, true));
    await waitFor(() => expect(mac.result.current).toEqual({ files: 1, sandbox: null }));
    expect(getResources).not.toHaveBeenCalled();
  });

  it("reads the file count and the CPU, and reads no git or a refusing runner as nothing to show", async () => {
    getFiles.mockResolvedValue({ vcs: "git", files: [{ path: "a" }, { path: "b" }] });
    getResources.mockResolvedValue({ cpu_percent: 12.4 });
    const ok = renderHook(() => useEvidenceFacts("run-1", false));
    await waitFor(() => expect(ok.result.current).toEqual({ files: 2, sandbox: "12%" }));
    getFiles.mockResolvedValue({ vcs: "none", files: [] });
    getResources.mockRejectedValue(new HttpError(409, "no sandbox"));
    const none = renderHook(() => useEvidenceFacts("run-2", false));
    await waitFor(() => expect(none.result.current).toEqual({ files: null, sandbox: null }));
  });

  // UXR-41: one failed read is not a verdict.
  it("a first failed read says it could not load, not Not inspectable or Not available", async () => {
    getFiles.mockRejectedValue(new Error("network"));
    getResources.mockRejectedValue(new HttpError(502, "bad gateway"));
    const hook = renderHook(() => useEvidenceFacts("run-3", false));
    await waitFor(() => expect(hook.result.current).toEqual({ files: null, sandbox: null, failed: { files: true, sandbox: true } }));
    render(<RunStatusStrip ctx={ctx({ evidence: hook.result.current })} />);
    expect(tile("Files").getByText("Couldn't load right now.")).toBeInTheDocument();
    expect(tile("Sandbox").getByText("Couldn't load right now.")).toBeInTheDocument();
    expect(screen.queryByText("Not inspectable")).toBeNull();
    expect(screen.queryByText("Not available")).toBeNull();
  });

  it("keeps the last good value when a later read fails", async () => {
    getFiles.mockResolvedValue({ vcs: "git", files: [{ path: "a" }, { path: "b" }] });
    getResources.mockResolvedValue({ cpu_percent: 12.4 });
    const hook = renderHook(({ id }) => useEvidenceFacts(id, false), { initialProps: { id: "run-4" } });
    await waitFor(() => expect(hook.result.current).toEqual({ files: 2, sandbox: "12%" }));
    getFiles.mockRejectedValue(new Error("network"));
    getResources.mockRejectedValue(new Error("network"));
    // A new id re-runs the read; the state, and with it the last good value, carries over.
    hook.rerender({ id: "run-4b" });
    await waitFor(() => expect(getFiles).toHaveBeenCalledTimes(2));
    await act(async () => {
      await Promise.resolve();
    });
    expect(hook.result.current).toEqual({ files: 2, sandbox: "12%" });
  });
});
