/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { launchGateOpen, PREFLIGHT_FRESH_MS, type PreflightRead } from "../draft/launch-gate";
import { StepShell } from "../steps/step-shell";

const NOW = 1_800_000_000_000;

function Review({ blockers = 0, preflight, generation = 7, onLaunch }: { blockers?: number; preflight: PreflightRead | null; generation?: number; onLaunch: () => void }) {
  const open = launchGateOpen({ blockers, preflight, generation, now: NOW });
  return (
    <StepShell title="Review" primary={{ label: "Launch", onActivate: onLaunch, disabled: !open, describedBy: "nr-blockers" }}>
      <div id="nr-blockers" role="status">{open ? "" : "Check the server's current result again."}</div>
    </StepShell>
  );
}
const ctrlEnter = () => fireEvent.keyDown(document, { key: "Enter", ctrlKey: true });
const cmdEnter = () => fireEvent.keyDown(document, { key: "Enter", metaKey: true });

afterEach(cleanup);

describe("Launch gate (U11)", () => {
  it.each<[string, PreflightRead | null, number]>([
    ["no preflight answer", null, 0],
    ["an answer older than a minute", { generation: 7, at: NOW - PREFLIGHT_FRESH_MS }, 0],
    ["an answer for another body", { generation: 6, at: NOW - 1000 }, 0],
    ["a fresh, current answer beside a blocker", { generation: 7, at: NOW - 1000 }, 1],
  ])("%s keeps Launch disabled and Ctrl/Cmd+Enter does nothing", (_case, preflight, blockers) => {
    const onLaunch = vi.fn();
    render(<Review preflight={preflight} blockers={blockers} onLaunch={onLaunch} />);
    const launch = screen.getByRole("button", { name: "Launch" });
    expect(launch).toBeDisabled();
    ctrlEnter();
    cmdEnter();
    fireEvent.click(launch);
    expect(onLaunch).not.toHaveBeenCalled();
  });

  it("describes a held Launch by the blocker list, an element that exists", () => {
    render(<Review preflight={null} onLaunch={vi.fn()} />);
    const id = screen.getByRole("button", { name: "Launch" }).getAttribute("aria-describedby");
    expect(id).toBe("nr-blockers");
    expect(document.getElementById(id ?? "")).toHaveTextContent("Check the server's current result again.");
  });

  it("opens with no blockers and a current answer under a minute old, by click and by Ctrl/Cmd+Enter", () => {
    const onLaunch = vi.fn();
    render(<Review preflight={{ generation: 7, at: NOW - PREFLIGHT_FRESH_MS + 1 }} onLaunch={onLaunch} />);
    expect(screen.getByRole("button", { name: "Launch" })).toBeEnabled();
    ctrlEnter();
    cmdEnter();
    fireEvent.click(screen.getByRole("button", { name: "Launch" }));
    expect(onLaunch).toHaveBeenCalledTimes(3);
  });

  it("Ctrl/Cmd+Enter does nothing while a dialog is open", () => {
    const onLaunch = vi.fn();
    render(
      <>
        <Review preflight={{ generation: 7, at: NOW - 1000 }} onLaunch={onLaunch} />
        <div role="dialog" />
      </>,
    );
    ctrlEnter();
    expect(onLaunch).not.toHaveBeenCalled();
  });
});
