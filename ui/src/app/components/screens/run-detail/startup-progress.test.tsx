/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, act } from "@testing-library/react";
import type { AgentRun } from "../../../lib/types";
import { SIGNIN_PROGRESS } from "../settings/login-pane-copy";
import { PENDING_NO_DETAIL, RUN_STARTUP } from "../run-status-detail";
import { StartupProgress } from "./startup-progress";

const T0 = Date.parse("2026-09-29T12:00:00Z");
const iso = (ms: number) => new Date(ms).toISOString();

function run(over: Partial<AgentRun>): AgentRun {
  return {
    id: "run-1",
    state: "STARTING",
    created_at: iso(T0),
    updated_at: iso(T0),
    ...over,
  } as AgentRun;
}

const states = () => screen.getAllByRole("listitem").map((li) => li.getAttribute("data-state"));

afterEach(() => vi.useRealTimers());

describe("StartupProgress", () => {
  it("draws the rows with data-state and aria-current, inside a dark token scope", () => {
    vi.useFakeTimers({ now: T0 + 3000 });
    render(<StartupProgress lastStep="terminal" run={run({ status_detail: "image: Pulling: x", status_reason: "Pulling" })} />);
    const list = screen.getByTestId("run-startup-progress");
    expect(states()).toEqual(["done", "active", "pending"]);
    const items = screen.getAllByRole("listitem");
    expect(items[1]).toHaveAttribute("aria-current", "step");
    expect(items[0]).not.toHaveAttribute("aria-current");
    expect(list.closest(".dark")).not.toBeNull();
    expect(screen.getByRole("status")).toHaveTextContent(SIGNIN_PROGRESS.DOWNLOAD_HINT);
  });

  it("never repeats the header's Queued line", () => {
    vi.useFakeTimers({ now: T0 + 70_000 });
    render(<StartupProgress lastStep="task" run={run({ state: "PENDING" })} />);
    expect(screen.queryByText(PENDING_NO_DETAIL)).not.toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent(RUN_STARTUP.SLOW);
  });

  it("the clock ticks: the slow line appears at 60 s with no new run data", () => {
    vi.useFakeTimers({ now: T0 + 58_000 });
    render(<StartupProgress lastStep="terminal" run={run({})} />);
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    act(() => void vi.advanceTimersByTime(3000));
    expect(screen.getByRole("status")).toHaveTextContent(RUN_STARTUP.SLOW);
  });

  it("a failed pull draws the failed row and an alert, no status hint", () => {
    vi.useFakeTimers({ now: T0 });
    render(
      <StartupProgress
        lastStep="terminal"
        run={run({ status_detail: "agent: ImagePullBackOff: denied", status_reason: "ImagePullBackOff" })}
      />,
    );
    expect(states()).toEqual(["done", "failed"]);
    expect(screen.getByRole("alert")).toHaveTextContent("The image could not be pulled: denied");
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("remembers a Building read: Build ticks to done when the run moves to STARTING, and resets for another run", () => {
    vi.useFakeTimers({ now: T0 + 5000 });
    const pending = run({ state: "PENDING", status_detail: "image: Building", status_reason: "Building" });
    const { rerender } = render(<StartupProgress lastStep="terminal" run={pending} />);
    expect(states()).toEqual(["active", "pending", "pending", "pending"]);

    rerender(<StartupProgress lastStep="terminal" run={run({ state: "STARTING", updated_at: iso(T0 + 4000) })} />);
    expect(states()).toEqual(["done", "active", "pending", "pending"]);

    // A different run in the same slot carries no memory of the first one's build.
    rerender(<StartupProgress lastStep="terminal" run={run({ id: "run-2", state: "STARTING", updated_at: iso(T0 + 4000) })} />);
    expect(states()).toEqual(["active", "pending", "pending"]);
  });

  it("renders nothing for a run that is up", () => {
    const { container } = render(<StartupProgress lastStep="terminal" run={run({ state: "RUNNING" })} />);
    expect(container).toBeEmptyDOMElement();
  });
});
