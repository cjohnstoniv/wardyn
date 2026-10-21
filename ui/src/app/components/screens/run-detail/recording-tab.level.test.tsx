/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AuditEvent } from "../../../lib/types";

const getDesktopRecording = vi.fn();
vi.mock("../../../lib/api/recordings", () => ({
  recordings: { getDesktopRecording: (...a: unknown[]) => getDesktopRecording(...a) },
}));

import { RecordingTab } from "./recording-tab";

const RUN = "11111111-1111-4111-8111-111111111111";
const KEY = `${RUN}~desktop-abc`;
const ev = (action: string, data: Record<string, unknown>, target = RUN): AuditEvent =>
  ({ id: action, time: "2026-10-20T10:44:32Z", actor: "wardynd", action, target, outcome: "success", data }) as AuditEvent;

function tab(props: Partial<React.ComponentProps<typeof RecordingTab>>) {
  return render(
    <MemoryRouter>
      <RecordingTab state="ready" recording={null} recordingDisabled={false} runId={RUN} sessions={[]} selected={RUN}
        onSelect={() => {}} onRetry={() => {}} {...props} />
    </MemoryRouter>,
  );
}

describe("RecordingTab levels (#2968, M7 RunStrip, Replay, States 8)", () => {
  it("names the level the run is recorded at", () => {
    tab({ level: "screen_actions" });
    expect(screen.getByTestId("recording-level-strip")).toHaveTextContent("Recording · Screen and actions");
  });

  it("a run resolved to off says so instead of a missing session", () => {
    tab({ level: "off", notRecorded: "policy_off" });
    expect(screen.getByTestId("recording-level-strip")).toHaveTextContent(
      "Recording offNothing is captured. The audit log still shows what happened.",
    );
    expect(screen.getByText("This run's policy set recording to Off. The audit log still shows what happened.")).toBeInTheDocument();
  });

  it("a desktop recording shows its gap and the tamper-evident rows Wardyn wrote", async () => {
    getDesktopRecording.mockResolvedValue({
      raw: "{}",
      playback: { version: 1, format: "novnc-playback", encoding: "base64", started_at: "", frames: ["{0{AA=="],
        gaps: [{ start_ms: 1000, end_ms: 135_000, reason: "you took over to sign in" }] },
    });
    const audit = [
      ev("desktop.recording.write", { key: KEY }, KEY),
      ev("desktop.recording.resume", { gap_start: "2026-10-20T10:42:18Z", gap_end: "2026-10-20T10:44:32Z", gap_ms: 134_000, reason: "you took over to sign in" }),
    ];
    tab({ level: "screen_actions", recordingAudit: audit, selected: KEY });
    const gap = await screen.findByTestId("desktop-replay-gap");
    expect(gap).toHaveTextContent("Recording paused for 2 min 14 s");
    expect(gap).toHaveTextContent("You took over to sign in, so no screen frames were kept. The action log kept going. The gap is marked in the audit log.");
    const row = screen.getByTestId("desktop-replay-log-row");
    expect(within(row).getByText("Wardyn, not the desktop")).toBeInTheDocument();
    expect(screen.getByText("Keystrokes are never recorded.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Download recording" })).toBeInTheDocument();
  });
});
