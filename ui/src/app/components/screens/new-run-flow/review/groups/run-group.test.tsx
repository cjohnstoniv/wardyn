/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Review's "Run and timing" row for a run that starts later.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { whenText } from "../../../../../lib/schedule";
import type { SetupStatus } from "../../../../../lib/types/setup";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { ModelAccessProvider } from "../../../../wardyn/model-access-context";
import { initialDraft, type NewRunDraft } from "../../draft/run-intent";
import { RunGroup } from "./run-group";

afterEach(cleanup);

const ON = { features: { scheduled_runs: true } } as SetupStatus;
const tomorrow = () => new Date(Date.now() + 86_400_000).toISOString().slice(0, 10);

function show(draft: NewRunDraft, status: SetupStatus | null = ON) {
  render(
    <MemoryRouter>
      <ModelAccessProvider status={status} onRefresh={() => {}}>
        <RunGroup draft={draft} pre={null} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />
      </ModelAccessProvider>
    </MemoryRouter>,
  );
}
const autonomous = (schedule?: NewRunDraft["schedule"]): NewRunDraft => ({
  ...initialDraft("alice"), intent: { shape: "agent_task", experience: "background" }, schedule,
});

describe("Review: Run and timing", () => {
  it("says Autonomous · Starts {when}, then that it is checked again, for a good start", () => {
    show(autonomous({ date: tomorrow(), time: "02:00", zone: "UTC" }));
    const when = whenText(Date.parse(`${tomorrow()}T02:00:00Z`), "UTC");
    expect(screen.getByText(NR_SHARED.JOIN(NR_REVIEW.TIMING_STARTS(when), NR_REVIEW.TIMING_CHECKED))).toBeInTheDocument();
  });

  it("asks for a start time while the date or time is missing or not allowed", () => {
    show(autonomous({ date: "", time: "", zone: "UTC" }));
    expect(screen.getByText(NR_REVIEW.TIMING_CHOOSE)).toBeInTheDocument();
    cleanup();
    show(autonomous({ date: "2020-01-01", time: "02:00", zone: "UTC" }));
    expect(screen.getByText(NR_REVIEW.TIMING_CHOOSE)).toBeInTheDocument();
  });

  it("keeps Autonomous · Run now with no start, and when the switch is off or absent", () => {
    show(autonomous());
    expect(screen.getByText(NR_REVIEW.TIMING_NOW)).toBeInTheDocument();
    cleanup();
    show(autonomous({ date: tomorrow(), time: "02:00", zone: "UTC" }), null);
    expect(screen.getByText(NR_REVIEW.TIMING_NOW)).toBeInTheDocument();
    expect(screen.queryByText(NR_REVIEW.TIMING_CHECKED, { exact: false })).not.toBeInTheDocument();
  });
});

describe("Review: Keep it running", () => {
  const script = (service?: NewRunDraft["intent"]["service"], over: NewRunDraft["intent"] = {}): NewRunDraft => ({
    ...initialDraft("alice"),
    intent: { shape: "application", experience: "background", workload: { kind: "command", command: "./serve" }, service, ...over },
  });

  it("says the service on its own line, with its restart and its check", () => {
    show(script({ restart: "on_failure", port: 8080, health: { path: "/healthz", interval_sec: 15, failure_threshold: 3 } }));
    expect(screen.getByText("Service · restarts when it fails · checks port 8080 /healthz every 15 s")).toBeInTheDocument();
    cleanup();
    show(script({ restart: "never", port: 8080, health: { interval_sec: 30, failure_threshold: 3 } }));
    expect(screen.getByText("Service · doesn't restart · checks port 8080 every 30 s")).toBeInTheDocument();
  });

  it("says nothing for a run with no service, or one that cannot carry it", () => {
    show(script(undefined));
    expect(screen.queryByText(/^Service ·/)).not.toBeInTheDocument();
    cleanup();
    show(script({ restart: "on_failure" }, { machine_kind: "linux_vm" }));
    expect(screen.queryByText(/^Service ·/)).not.toBeInTheDocument();
  });
});
