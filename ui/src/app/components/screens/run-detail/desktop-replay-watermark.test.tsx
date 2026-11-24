/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

const getDesktopRecording = vi.fn();
const verifyChain = vi.fn();
vi.mock("../../../lib/api/recordings", () => ({
  recordings: {
    getDesktopRecording: (...a: unknown[]) => getDesktopRecording(...a),
    verifyChain: (...a: unknown[]) => verifyChain(...a),
  },
}));
const listAudit = vi.hoisted(() => vi.fn());
vi.mock("../../../lib/api/audit", () => ({ audit: { listAudit: (...a: unknown[]) => listAudit(...a) } }));
// Stands in for noVNC's RFB, which owns the screen box and empties it when it draws.
vi.mock("@novnc/novnc", () => ({
  default: class {
    viewOnly = false;
    constructor(target: HTMLElement, channel: { onmessage: ((e: { data: ArrayBuffer }) => void) | null }) {
      channel.onmessage = () => {};
      target.appendChild(document.createElement("canvas"));
    }
    disconnect() {}
  },
}));

import { DESKTOP } from "../../wardyn/copy/desktop";
import { RECORDING_REPLAY } from "../../wardyn/copy/recording-level";
import type { AuditEvent } from "../../../lib/types";
import { RecordingTab } from "./recording-tab";

const RUN = "3f2c9a1e-7b44-4c0d-9e21-5a6b8d0c1f73";
const KEY = `${RUN}~desktop-abc`;
const WHO = "alex@example.com";
const RAW = '{"version":1}';
const WRITTEN = { id: "w", time: "2026-10-20T10:50:00Z", actor_type: "system", actor: "wardynd", action: "desktop.recording.write", target: KEY, outcome: "success", data: {} } as AuditEvent;

function tab(watermark?: { who: string; run: string; required: boolean }) {
  return render(
    <MemoryRouter>
      <RecordingTab state="ready" recording={null} recordingDisabled={false} runId={RUN} sessions={[]} selected={KEY}
        onSelect={() => {}} onRetry={() => {}} level="screen_actions" screen recordingAudit={[WRITTEN]} watermark={watermark} />
    </MemoryRouter>,
  );
}

const toggle = () => screen.queryByRole("button", { name: DESKTOP.watermarkToggle });

beforeEach(() => {
  listAudit.mockResolvedValue([]);
  verifyChain.mockResolvedValue({ ok: true, first_seq: 1, last_seq: 2, checked: 2, marks: [] });
  getDesktopRecording.mockResolvedValue({
    raw: RAW,
    playback: { version: 1, format: "novnc-playback", encoding: "base64", started_at: "2026-10-20T10:40:00Z", frames: ["{0{AQ=="], gaps: [] },
  });
});
afterEach(cleanup);

describe("DesktopReplay watermark", () => {
  it("shows no toggle and no layer without a watermark on the run", async () => {
    tab();
    await screen.findByTestId("desktop-replay-screen");
    expect(toggle()).toBeNull();
    expect(screen.queryByText(DESKTOP.watermarkRequired)).toBeNull();
    expect(screen.queryByTestId("desktop-watermark")).toBeNull();
  });

  it("optional: off to start, then the toggle button turns the layer on and off", async () => {
    tab({ who: WHO, run: RUN, required: false });
    await screen.findByTestId("desktop-replay-screen");
    expect(toggle()).toHaveAttribute("aria-pressed", "false");
    expect(screen.queryByTestId("desktop-watermark")).toBeNull();
    fireEvent.click(toggle()!);
    expect(toggle()).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByTestId("desktop-watermark")).toHaveTextContent(`${WHO} · ${RUN}`);
    fireEvent.click(toggle()!);
    expect(screen.queryByTestId("desktop-watermark")).toBeNull();
  });

  it("required: the layer is on, with the required line and no toggle", async () => {
    tab({ who: WHO, run: RUN, required: true });
    await screen.findByTestId("desktop-replay-screen");
    expect(screen.getByTestId("desktop-watermark")).toBeInTheDocument();
    expect(screen.getByText(DESKTOP.watermarkRequired)).toBeInTheDocument();
    expect(toggle()).toBeNull();
  });

  it("the layer is a sibling of the screen box, not inside it, and stays when noVNC redraws the box", async () => {
    tab({ who: WHO, run: RUN, required: true });
    const box = await screen.findByTestId("desktop-replay-screen");
    await waitFor(() => expect(box.querySelector("canvas")).not.toBeNull());
    const layer = screen.getByTestId("desktop-watermark");
    expect(box).not.toContainElement(layer);
    expect(layer.parentElement).toBe(box.parentElement);
    expect(box.children).toHaveLength(1);
    expect(box.className).not.toContain("watermark");
  });

  it("covers the screen box only, not the controls row, and the download carries no mark", async () => {
    tab({ who: WHO, run: RUN, required: true });
    await screen.findByTestId("desktop-replay-screen");
    const layer = screen.getByTestId("desktop-watermark");
    const download = screen.getByRole("button", { name: RECORDING_REPLAY.DOWNLOAD });
    expect(layer.parentElement).not.toContainElement(download);
    let blob: Blob | undefined;
    const createObjectURL = vi.fn((b: Blob) => {
      blob = b;
      return "blob:x";
    });
    const url = URL as unknown as Record<string, unknown>;
    const before = { create: url.createObjectURL, revoke: url.revokeObjectURL };
    url.createObjectURL = createObjectURL;
    url.revokeObjectURL = vi.fn();
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    try {
      fireEvent.click(download);
      expect(createObjectURL).toHaveBeenCalledTimes(1);
      expect(await blob!.text()).toBe(RAW);
    } finally {
      click.mockRestore();
      url.createObjectURL = before.create;
      url.revokeObjectURL = before.revoke;
    }
  });
});
