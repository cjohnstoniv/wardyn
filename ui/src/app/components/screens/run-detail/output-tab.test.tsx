/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Mock packet M8: the Output tab shows every state with the canon strings,
// renders sandbox-controlled bytes as literal text, and stops polling once the
// capture is complete.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, act } from "@testing-library/react";
import { HttpError } from "../../../lib/api/core";
import type { RunOutput } from "../../../lib/types";
import { RUN_COCKPIT, RUN_OUTPUT } from "../../wardyn/copy";
import { OutputTab } from "./output-tab";

const getMock = vi.fn();
vi.mock("../../../lib/api/run-output", () => ({
  runOutput: { get: (...a: unknown[]) => getMock(...a) },
}));

const out = (o: Partial<RunOutput> = {}): RunOutput => ({
  output: "$ go test ./...\nok",
  truncated: false,
  complete: true,
  source: "stdout",
  incomplete: false,
  capture_gap: false,
  mask_scope: "run",
  captured_at: "2026-10-03T14:02:00Z",
  ...o,
});
const refuse = (reason: string, status = 404) =>
  getMock.mockRejectedValue(new HttpError(status, "refused", reason));

async function mount(props: Partial<React.ComponentProps<typeof OutputTab>> = {}) {
  const r = render(<OutputTab runId="run-1" live={false} onGoRecording={() => {}} {...props} />);
  await act(async () => {});
  return r;
}

beforeEach(() => {
  getMock.mockReset();
});
afterEach(() => {
  vi.useRealTimers();
});

describe("OutputTab — kept output", () => {
  it("live: source, live chip, no captured time, empty-live text, CLI hint", async () => {
    getMock.mockResolvedValue(out({ complete: false, output: "", captured_at: undefined }));
    await mount({ live: true });
    expect(screen.getByText(RUN_OUTPUT.sourceStdout)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.live)).toBeInTheDocument();
    expect(screen.queryByText(/^Captured /)).toBeNull();
    expect(screen.getByText(RUN_OUTPUT.emptyLive)).toBeInTheDocument();
    expect(screen.getByText(`wardyn run output run-1`)).toBeInTheDocument();
  });

  it("final: final chip, captured time, the bytes, a Copy control", async () => {
    getMock.mockResolvedValue(out());
    await mount();
    expect(screen.getByText(RUN_OUTPUT.final)).toBeInTheDocument();
    expect(screen.getByText(/^Captured /)).toBeInTheDocument();
    expect(screen.getByTestId("run-output-text").textContent).toBe("$ go test ./...\nok");
    expect(screen.getByRole("button", { name: RUN_OUTPUT.copyLabel })).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("final and empty says the run printed nothing", async () => {
    getMock.mockResolvedValue(out({ output: "" }));
    await mount();
    expect(screen.getByText(RUN_OUTPUT.emptyFinal)).toBeInTheDocument();
  });

  it("every notice, in the M8 order: globals_only, gap, incomplete, truncated", async () => {
    getMock.mockResolvedValue(
      out({ mask_scope: "globals_only", capture_gap: true, incomplete: true, truncated: true }),
    );
    const { container } = await mount();
    const text = container.textContent ?? "";
    const at = [RUN_OUTPUT.globalsOnly, RUN_OUTPUT.captureGap, RUN_OUTPUT.incomplete, RUN_OUTPUT.truncated].map((s) =>
      text.indexOf(s),
    );
    expect(at.every((i) => i >= 0)).toBe(true);
    expect([...at].sort((a, b) => a - b)).toEqual(at);
  });

  it("mask_scope run shows no globals_only notice", async () => {
    getMock.mockResolvedValue(out());
    await mount();
    expect(screen.queryByText(RUN_OUTPUT.globalsOnly)).toBeNull();
  });

  it("pane snapshot: Last screen chip and the trust caption", async () => {
    getMock.mockResolvedValue(out({ source: "pane_snapshot" }));
    await mount();
    expect(screen.getByText(RUN_OUTPUT.sourcePane)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.paneCaption)).toBeInTheDocument();
    expect(screen.queryByText(RUN_OUTPUT.sourceStdout)).toBeNull();
  });

  it("renders <script> markup and ANSI escapes as literal text", async () => {
    const hostile = "<script>alert(1)</script>\u001b[31mred\u001b[0m <img src=x onerror=alert(1)>";
    getMock.mockResolvedValue(out({ output: hostile }));
    await mount();
    const pre = screen.getByTestId("run-output-text");
    expect(pre.textContent).toBe(hostile);
    expect(pre.querySelector("script, img")).toBeNull();
    expect(pre.children.length).toBe(0);
  });
});

describe("OutputTab — polling", () => {
  it("polls while incomplete and stops once complete is true", async () => {
    vi.useFakeTimers();
    getMock
      .mockResolvedValueOnce(out({ complete: false, captured_at: undefined }))
      .mockResolvedValueOnce(out({ complete: false, captured_at: undefined }))
      .mockResolvedValue(out());
    await mount({ live: true });
    expect(getMock).toHaveBeenCalledTimes(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4000);
    });
    expect(getMock).toHaveBeenCalledTimes(2);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4000);
    });
    expect(getMock).toHaveBeenCalledTimes(3);
    expect(screen.getByText(RUN_OUTPUT.final)).toBeInTheDocument();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(20_000);
    });
    expect(getMock).toHaveBeenCalledTimes(3);
  });
});

describe("OutputTab — refusals", () => {
  it("off: names the env var in mono", async () => {
    refuse("run_output_off", 404);
    await mount();
    expect(screen.getByText(RUN_OUTPUT.offTitle)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.offEnvVar).className).toContain("font-mono");
  });

  it("not kept: a long-ended run says nothing was kept", async () => {
    refuse("run_output_not_kept");
    await mount({ endedAt: "2020-01-01T00:00:00Z" });
    expect(screen.getByText(RUN_OUTPUT.notKeptTitle)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.notKeptDesc)).toBeInTheDocument();
  });

  it("not kept: a live run says it is saving and keeps polling", async () => {
    vi.useFakeTimers();
    refuse("run_output_not_kept");
    await mount({ live: true });
    expect(screen.getByText(RUN_OUTPUT.savingTitle)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.savingDesc)).toBeInTheDocument();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4000);
    });
    expect(getMock).toHaveBeenCalledTimes(2);
  });

  it("not kept: a run that ended seconds ago says it is saving", async () => {
    refuse("run_output_not_kept");
    await mount({ endedAt: new Date(Date.now() - 5000).toISOString() });
    expect(screen.getByText(RUN_OUTPUT.savingTitle)).toBeInTheDocument();
  });

  it("not kept: the saving text switches to nothing-kept once the window closes, and polling stops", async () => {
    vi.useFakeTimers();
    refuse("run_output_not_kept");
    await mount({ endedAt: new Date(Date.now() - 58_000).toISOString() });
    expect(screen.getByText(RUN_OUTPUT.savingTitle)).toBeInTheDocument();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(20_000);
    });
    expect(screen.getByText(RUN_OUTPUT.notKeptTitle)).toBeInTheDocument();
    const calls = getMock.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(20_000);
    });
    expect(getMock).toHaveBeenCalledTimes(calls);
  });

  it("expired", async () => {
    refuse("run_output_expired", 410);
    await mount();
    expect(screen.getByText(RUN_OUTPUT.expiredTitle)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.expiredDesc)).toBeInTheDocument();
  });

  it("erased", async () => {
    refuse("run_output_erased");
    await mount();
    expect(screen.getByText(RUN_OUTPUT.erasedTitle)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.erasedDesc)).toBeInTheDocument();
  });

  it("interactive without a snapshot links to the Recording tab", async () => {
    refuse("run_output_interactive");
    const goRecording = vi.fn();
    await mount({ onGoRecording: goRecording });
    expect(screen.getByText(RUN_OUTPUT.interactiveTitle)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.interactiveDesc)).toBeInTheDocument();
    screen.getByRole("button", { name: RUN_OUTPUT.interactiveLink }).click();
    expect(goRecording).toHaveBeenCalledTimes(1);
  });

  it("503 mask_state_unavailable: the load-error title and the mask line", async () => {
    refuse("mask_state_unavailable", 503);
    await mount();
    expect(screen.getByText(RUN_COCKPIT.loadError)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.maskUnavailable)).toBeInTheDocument();
  });

  it("any other failure: the load-error message with a retry", async () => {
    getMock.mockRejectedValue(new Error("boom"));
    await mount();
    expect(screen.getByText(RUN_COCKPIT.loadError)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
  });
});
