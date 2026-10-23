/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { RunRef, shortRunId } from "./run-ref";

const ID = "3f9c1e2a-7b44-4d0e-9c1a-5e8b2d6f4a10";

afterEach(() => vi.restoreAllMocks());

describe("RunRef (#3036)", () => {
  it("leads with the title and follows with the 8-character id, whole id on hover", () => {
    render(<RunRef runId={ID} title="payments deps bump" />);
    const id = screen.getByRole("button", { name: "3f9c1e2a" });
    expect(id).toHaveAttribute("title", `${ID}\nClick to copy`);
    expect(screen.getByText("payments deps bump")).toBeInTheDocument();
    expect(screen.queryByText(ID)).toBeNull();
  });

  it("a click on the id copies the whole id and never reaches the row", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const rowClick = vi.fn();
    render(
      <div onClick={rowClick}>
        <RunRef runId={ID} title="t" />
      </div>,
    );
    await userEvent.click(screen.getByRole("button", { name: "3f9c1e2a" }));
    expect(writeText).toHaveBeenCalledWith(ID);
    expect(await screen.findByText("Copied")).toBeInTheDocument();
    expect(rowClick).not.toHaveBeenCalled();
  });

  it("with no title the id stands alone after 'Run'", () => {
    render(<RunRef runId={ID} />);
    expect(screen.getByText("Run")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "3f9c1e2a" })).toBeInTheDocument();
  });

  it("names a long task by its first line only", () => {
    render(<RunRef runId={ID} title={"Fix the flaky checkout test\nsecond line"} />);
    expect(screen.getByText("Fix the flaky checkout test")).toBeInTheDocument();
    expect(screen.queryByText(/second line/)).toBeNull();
  });

  it("is plain text on the run's own page, a link or a button elsewhere", async () => {
    const onOpen = vi.fn();
    const { rerender } = render(
      <MemoryRouter>
        <RunRef runId={ID} title="x" />
      </MemoryRouter>,
    );
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getAllByRole("button")).toHaveLength(1);
    rerender(
      <MemoryRouter>
        <RunRef runId={ID} title="x" to={`/runs/${ID}`} />
      </MemoryRouter>,
    );
    expect(screen.getByRole("link", { name: "x" })).toHaveAttribute("href", `/runs/${ID}`);
    rerender(
      <MemoryRouter>
        <RunRef runId={ID} title="x" onOpen={onOpen} />
      </MemoryRouter>,
    );
    await userEvent.click(screen.getByRole("button", { name: "x" }));
    expect(onOpen).toHaveBeenCalledTimes(1);
  });

  it("drops a run_ prefix before shortening", () => {
    expect(shortRunId(`run_${ID}`)).toBe("3f9c1e2a");
  });
});
