/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A macOS VM's start phases on the run page (mock M-D1 frames 1 to 4).
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import type { AgentRun } from "../../../lib/types";
import { RUN_STARTUP, RUN_STARTUP_MACOS } from "../run-status-detail";
import { StartupProgress } from "./startup-progress";

const getSetupStatus = vi.hoisted(() => vi.fn());
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus } }));
beforeEach(() => getSetupStatus.mockResolvedValue({ runner: {} }));
afterEach(() => vi.useRealTimers());

const T0 = Date.UTC(2000, 0, 1, 12);
const iso = (ms: number) => new Date(ms).toISOString();
const run = { id: "run-1", state: "STARTING", created_at: iso(T0), updated_at: iso(T0), machine_kind: "macos_vm" } as AgentRun;

const rows = () => screen.getAllByRole("listitem").map((li) => [li.textContent, li.getAttribute("data-state")]);

describe("StartupProgress: macOS VM", () => {
  it("frame 1: cloning is done, macOS is starting, the rest wait, and the task row is today's", () => {
    vi.useFakeTimers({ now: T0 + 3000 });
    render(<StartupProgress lastStep="task" run={run} macOS={{ phase: "start" }} />);
    expect(rows()).toEqual([
      [RUN_STARTUP_MACOS.STEP_CLONE, "done"],
      [RUN_STARTUP_MACOS.STEP_START, "active"],
      [RUN_STARTUP_MACOS.STEP_CONNECT, "pending"],
      [RUN_STARTUP.STEP_TASK, "pending"],
    ]);
    expect(screen.queryByText(RUN_STARTUP.STEP_START)).toBeNull();
  });

  it("frame 2: connecting to the VM is the active row", () => {
    vi.useFakeTimers({ now: T0 + 3000 });
    render(<StartupProgress lastStep="task" run={run} macOS={{ phase: "connect" }} />);
    expect(rows().map((r) => r[1])).toEqual(["done", "done", "active", "pending"]);
  });

  it("frame 3: the agent never connected, so the row fails and the alert says why; no task row", () => {
    vi.useFakeTimers({ now: T0 + 3000 });
    render(<StartupProgress lastStep="task" run={run} macOS={{ phase: "connect", failure: RUN_STARTUP_MACOS.NO_AGENT }} />);
    expect(rows()).toEqual([
      [RUN_STARTUP_MACOS.STEP_CLONE, "done"],
      [RUN_STARTUP_MACOS.STEP_START, "done"],
      [RUN_STARTUP_MACOS.STEP_CONNECT_FAILED, "failed"],
    ]);
    expect(screen.getByRole("alert")).toHaveTextContent(RUN_STARTUP_MACOS.NO_AGENT);
  });

  it("frame 4: a start refused for disk fails the Starting macOS row and names the figures", () => {
    vi.useFakeTimers({ now: T0 + 3000 });
    render(<StartupProgress lastStep="terminal" run={run} macOS={{ phase: "start", failure: RUN_STARTUP_MACOS.DISK_SHORT("51", "38") }} />);
    expect(rows()).toEqual([
      [RUN_STARTUP_MACOS.STEP_CLONE, "done"],
      [RUN_STARTUP_MACOS.STEP_START_FAILED, "failed"],
    ]);
    expect(screen.getByRole("alert")).toHaveTextContent("Not enough free disk to start a macOS VM safely: it reserves 51 GB and this Mac has 38 GB free.");
  });

  it("keeps the container rows for a run with no macOS phase", () => {
    vi.useFakeTimers({ now: T0 + 3000 });
    render(<StartupProgress lastStep="terminal" run={run} />);
    expect(rows().map((r) => r[0])).toContain(RUN_STARTUP.STEP_START);
  });
});
