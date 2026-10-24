/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AuditEvent } from "../../../lib/types";
import { clockTime } from "../../../lib/format";

const getDesktopRecording = vi.fn();
const verifyChain = vi.fn();
vi.mock("../../../lib/api/recordings", () => ({
  recordings: {
    getDesktopRecording: (...a: unknown[]) => getDesktopRecording(...a),
    verifyChain: (...a: unknown[]) => verifyChain(...a),
  },
}));
vi.mock("../../../lib/api/audit", () => ({ audit: { listAudit: async () => [] } }));
// Stands in for noVNC's RFB: records what reaches the display through the channel it was handed.
const decoders = vi.hoisted(() => [] as { target: HTMLElement; got: number[]; gone: boolean }[]);
const rfbThrows = vi.hoisted(() => ({ on: false }));
vi.mock("@novnc/novnc", () => ({
  default: class {
    viewOnly = false;
    disconnect: () => void;
    constructor(target: HTMLElement, channel: { onmessage: ((e: { data: ArrayBuffer }) => void) | null }) {
      if (rfbThrows.on) throw new Error("noVNC refused the stream");
      const d = { target, got: [] as number[], gone: false };
      decoders.push(d);
      channel.onmessage = (e) => d.got.push(...new Uint8Array(e.data));
      this.disconnect = () => (d.gone = true);
    }
  },
}));

import { RUN_RECORDING } from "../../wardyn/copy/runners";
import { RecordingTab } from "./recording-tab";

const RUN = "11111111-1111-4111-8111-111111111111";
const KEY = `${RUN}~desktop-abc`;
const ev = (action: string, data: Record<string, unknown>, target = RUN, time = "2026-10-20T10:44:32Z"): AuditEvent =>
  ({ id: action, time, actor_type: "system", actor: "wardynd", action, target, outcome: "success", data }) as AuditEvent;

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

  it.each([
    ["runner_declined", RUN_RECORDING.runner_declined],
    ["org_off", RUN_RECORDING.org_off],
  ] as const)("a run with not_recorded=%s says it was not recorded, whatever its level", (notRecorded, note) => {
    tab({ level: "terminal", notRecorded });
    const strip = screen.getByTestId("recording-level-strip");
    expect(strip).toHaveTextContent("Recording off");
    expect(strip).toHaveTextContent(note);
    expect(strip).not.toHaveTextContent("Terminal only");
  });

  it("a desktop recording marks its gap, jumps from the action log and shows the chain verdict", async () => {
    getDesktopRecording.mockResolvedValue({
      raw: "{}",
      playback: { version: 1, format: "novnc-playback", encoding: "base64", started_at: "2026-10-20T10:40:00Z", frames: ["{0{AA==", "{600000{AA=="],
        gaps: [{ start_ms: 138_100, end_ms: 272_000, reason: "you took over to sign in" }] },
    });
    verifyChain.mockResolvedValue({ ok: true, first_seq: 4801, last_seq: 4838, checked: 38,
      marks: [{ id: "desktop.recording.pause", seq: 4815 }, { id: "desktop.recording.resume", seq: 4822 }] });
    const audit = [
      ev("desktop.recording.write", { key: KEY }, KEY, "2026-10-20T10:50:00Z"),
      ev("desktop.recording.pause", { gap_start: "2026-10-20T10:42:18Z", reason: "you took over to sign in" }, RUN, "2026-10-20T10:42:18Z"),
      ev("desktop.recording.resume", { gap_start: "2026-10-20T10:42:18Z", gap_end: "2026-10-20T10:44:32Z", gap_ms: 134_000, reason: "you took over to sign in" }),
    ];
    tab({ level: "screen_actions", recordingAudit: audit, selected: KEY });
    await screen.findByTestId("desktop-replay");
    expect(screen.queryByTestId("desktop-replay-gap")).toBeNull();
    expect(screen.getByTestId("desktop-replay-gap-mark")).toHaveTextContent("Gap 2 min 14 s");

    const rows = screen.getAllByTestId("desktop-replay-action");
    expect(rows.map((r) => within(r).getByText(/the screen recording$/).textContent)).toEqual(["paused the screen recording", "resumed the screen recording"]);
    fireEvent.click(rows[0]);
    expect(screen.getByTestId("desktop-replay-playhead")).toHaveAttribute("data-ms", "138000");
    expect(rows[0]).toHaveAttribute("aria-current", "true");
    const gap = screen.getByTestId("desktop-replay-gap");
    expect(gap).toHaveTextContent("Recording paused for 2 min 14 s");
    expect(gap).toHaveTextContent("You took over to sign in, so no screen frames were kept. The action log kept going. The gap is marked in the audit log.");
    fireEvent.click(within(gap).getByRole("button", { name: "Skip the gap" }));
    expect(screen.getByTestId("desktop-replay-playhead")).toHaveAttribute("data-ms", "272000");
    expect(screen.queryByTestId("desktop-replay-gap")).toBeNull();

    const chain = screen.getByTestId("desktop-replay-chain");
    expect(await within(chain).findByText("Chain intact")).toBeInTheDocument();
    expect(verifyChain).toHaveBeenCalledWith(RUN, KEY, "2026-10-20T10:40:00Z");
    expect(within(chain).getByTestId("desktop-replay-chain-verdict")).toHaveTextContent("Entries 4,801 to 4,838 are chained and none is missing.");
    const row = within(chain).getByTestId("desktop-replay-log-row");
    expect(row).toHaveTextContent("Entry 4,815 · ");
    expect(row).toHaveTextContent("Entry 4,822 · ");
    expect(within(row).getByText("Wardyn, not the desktop")).toBeInTheDocument();
    expect(screen.getByText("Keystrokes are never recorded.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Download recording" })).toBeInTheDocument();
  });

  it("the frames up to the playhead reach noVNC's display, and a jump back replays them on a fresh one", async () => {
    decoders.length = 0;
    getDesktopRecording.mockResolvedValue({
      raw: "{}",
      playback: { version: 1, format: "novnc-playback", encoding: "base64", started_at: "2026-10-20T10:40:00Z",
        frames: ["{0{AQ==", "{100000{Ag==", "{300000{Aw=="], gaps: [{ start_ms: 138_100, end_ms: 272_000, reason: "you took over to sign in" }] },
    });
    verifyChain.mockResolvedValue({ ok: true, first_seq: 1, last_seq: 2, checked: 2, marks: [] });
    const audit = [
      ev("desktop.recording.write", { key: KEY }, KEY, "2026-10-20T10:50:00Z"),
      ev("desktop.recording.pause", { gap_start: "2026-10-20T10:42:18Z", reason: "you took over to sign in" }, RUN, "2026-10-20T10:42:18Z"),
      ev("desktop.recording.resume", { gap_start: "2026-10-20T10:42:18Z", gap_end: "2026-10-20T10:44:32Z", gap_ms: 134_000, reason: "you took over to sign in" }),
    ];
    tab({ level: "screen_actions", recordingAudit: audit, selected: KEY });
    const screenEl = await screen.findByTestId("desktop-replay-screen");
    await waitFor(() => expect(decoders).toHaveLength(1));
    expect(decoders[0].target).toBe(screenEl);
    expect(decoders[0].got).toEqual([1]);

    const rows = screen.getAllByTestId("desktop-replay-action");
    fireEvent.click(rows[1]);
    expect(screen.getByTestId("desktop-replay-playhead")).toHaveAttribute("data-ms", "272000");
    expect(decoders[0].got).toEqual([1, 2]);
    fireEvent.click(rows[0]);
    expect(screen.getByTestId("desktop-replay-playhead")).toHaveAttribute("data-ms", "138000");
    expect(decoders.map((d) => [d.got, d.gone])).toEqual([[[1, 2], true], [[1, 2], false]]);
  });

  it("a decoder that throws drops only the screen: the replay stays and still seeks", async () => {
    rfbThrows.on = true;
    try {
      getDesktopRecording.mockResolvedValue({
        raw: "{}",
        playback: { version: 1, format: "novnc-playback", encoding: "base64", started_at: "2026-10-20T10:40:00Z",
          frames: ["{0{AQ==", "{100000{Ag=="], gaps: [{ start_ms: 138_100, end_ms: 272_000, reason: "you took over to sign in" }] },
      });
      verifyChain.mockResolvedValue({ ok: true, first_seq: 1, last_seq: 2, checked: 2, marks: [] });
      const audit = [
        ev("desktop.recording.write", { key: KEY }, KEY, "2026-10-20T10:50:00Z"),
        ev("desktop.recording.pause", { gap_start: "2026-10-20T10:42:18Z", reason: "you took over to sign in" }, RUN, "2026-10-20T10:42:18Z"),
        ev("desktop.recording.resume", { gap_start: "2026-10-20T10:42:18Z", gap_end: "2026-10-20T10:44:32Z", gap_ms: 134_000, reason: "you took over to sign in" }),
      ];
      tab({ level: "screen_actions", recordingAudit: audit, selected: KEY });
      expect(await screen.findByText(/^The screen could not be drawn here\./)).toBeInTheDocument();
      const rows = screen.getAllByTestId("desktop-replay-action");
      fireEvent.click(rows[1]);
      fireEvent.click(rows[0]);
      expect(screen.getByTestId("desktop-replay-playhead")).toHaveAttribute("data-ms", "138000");
      expect(screen.getByTestId("desktop-replay")).toBeInTheDocument();
    } finally {
      rfbThrows.on = false;
    }
  });

  it("a truncated or stopped recording says where the screen ends, and a whole one says nothing", async () => {
    verifyChain.mockResolvedValue({ ok: true, first_seq: 1, last_seq: 2, checked: 2, marks: [] });
    const audit = [ev("desktop.recording.write", { key: KEY }, KEY)];
    const doc = (extra: Record<string, unknown>) => ({
      raw: "{}",
      playback: { version: 1, format: "novnc-playback", encoding: "base64", started_at: "2026-10-20T10:40:00Z", frames: ["{0{AA==", "{60000{AA=="], ...extra },
    });
    const clock = (ms: number) => clockTime(new Date(Date.parse("2026-10-20T10:40:00Z") + ms).toISOString());

    getDesktopRecording.mockResolvedValue(doc({ truncated: true }));
    const first = tab({ level: "screen_actions", recordingAudit: audit, selected: KEY });
    expect(await screen.findByTestId("desktop-replay-stopped")).toHaveTextContent(`Screen recording stopped at ${clock(60_000)}; the action log continues.`);
    first.unmount();

    getDesktopRecording.mockResolvedValue(doc({ stopped_ms: 30_000 }));
    const second = tab({ level: "screen_actions", recordingAudit: audit, selected: KEY });
    expect(await screen.findByTestId("desktop-replay-stopped")).toHaveTextContent(`Screen recording stopped at ${clock(30_000)};`);
    second.unmount();

    getDesktopRecording.mockResolvedValue(doc({}));
    tab({ level: "screen_actions", recordingAudit: audit, selected: KEY });
    await screen.findByTestId("desktop-replay");
    expect(screen.queryByTestId("desktop-replay-stopped")).toBeNull();
  });

  it("a broken range says which entry does not verify", async () => {
    getDesktopRecording.mockResolvedValue({
      raw: "{}",
      playback: { version: 1, format: "novnc-playback", encoding: "base64", started_at: "2026-10-20T10:40:00Z", frames: ["{0{AA=="] },
    });
    verifyChain.mockResolvedValue({ ok: false, first_seq: 4801, last_seq: 4838, checked: 20, broken_seq: 4820, reason: "row_hash does not match", marks: [] });
    tab({ level: "screen_actions", recordingAudit: [ev("desktop.recording.write", { key: KEY }, KEY)], selected: KEY });
    const chain = await screen.findByTestId("desktop-replay-chain");
    expect(await within(chain).findByText("Chain broken")).toBeInTheDocument();
    expect(within(chain).getByTestId("desktop-replay-chain-verdict")).toHaveTextContent("Entry 4,820 does not verify: row_hash does not match.");
  });
});
