/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { WidgetContext } from "./widget-registry";

const getFiles = vi.fn();
const getResources = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: { getFiles: (...a: unknown[]) => getFiles(...a), getResources: (...a: unknown[]) => getResources(...a) },
}));

import { RunStatusStrip } from "./status-strip";

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
    getFiles.mockRejectedValue(new Error("501"));
    getResources.mockRejectedValue(new Error("501"));
    render(<RunStatusStrip ctx={ctx()} />);
    await screen.findAllByText("Not available");
    const labels = screen.getAllByTestId("status-tile").map((t) => t.firstElementChild?.textContent);
    expect(labels).toEqual(["Egress", "Files", "Sandbox", "Credentials"]);
  });

  it("the Sandbox tile says Not available, and Files says Not inspectable, when the runner offers nothing", async () => {
    getFiles.mockRejectedValue(new Error("501"));
    getResources.mockRejectedValue(new Error("409"));
    render(<RunStatusStrip ctx={ctx({ finished: true })} />);
    expect(await tile("Sandbox").findByText("Not available")).toBeInTheDocument();
    expect(tile("Files").getByText("Not inspectable")).toBeInTheDocument();
    expect(screen.queryByText("n/a")).toBeNull();
  });

  it("reads real evidence when there is some", async () => {
    getFiles.mockResolvedValue({ vcs: "git", files: [{ path: "a" }, { path: "b" }] });
    getResources.mockResolvedValue({ cpu_percent: 12.4 });
    render(<RunStatusStrip ctx={ctx()} />);
    expect(await tile("Files").findByText("2 changed")).toBeInTheDocument();
    expect(tile("Sandbox").getByText("12% CPU")).toBeInTheDocument();
  });

  it("Egress leads with what is held, Credentials counts grants and mints", async () => {
    getFiles.mockResolvedValue({ vcs: "git", files: [] });
    getResources.mockResolvedValue({});
    render(
      <RunStatusStrip
        ctx={ctx({
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
    getFiles.mockResolvedValue({ vcs: "git", files: [] });
    getResources.mockResolvedValue({});
    render(<RunStatusStrip ctx={ctx()} />);
    expect(tile("Egress").getByText("None yet")).toBeInTheDocument();
    await screen.findAllByText("Not available");
  });
});
