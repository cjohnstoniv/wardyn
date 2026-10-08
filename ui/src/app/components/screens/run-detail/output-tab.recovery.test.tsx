/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Mock packet M-O (#1831): output recovered from a recording says so and says
// it may not be everything; a capture gap never reads as a clean empty run; a
// refusal stays the refusal the server gave; Retry keeps its surface and focus.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, act, fireEvent, within } from "@testing-library/react";
import { HttpError } from "../../../lib/api/core";
import type { RunOutput } from "../../../lib/types";
import { aheadByHours } from "../../../lib/test-clock";
import { RUN_COCKPIT, RUN_OUTPUT } from "../../wardyn/copy";
import { OutputTab } from "./output-tab";

const getMock = vi.fn();
vi.mock("../../../lib/api/run-output", () => ({
  runOutput: { get: (...a: unknown[]) => getMock(...a) },
}));
vi.mock("../../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({}) },
}));

const writeText = vi.fn();

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
const recording = (o: Partial<RunOutput> = {}) => out({ source: "recording", incomplete: true, ...o });
const refusal = (reason: string, status: number) => new HttpError(status, "refused", reason);

async function mount(props: Partial<React.ComponentProps<typeof OutputTab>> = {}) {
  const r = render(<OutputTab runId="run-1" live={false} state="COMPLETED" onGoRecording={() => {}} {...props} />);
  await act(async () => {});
  return r;
}
const advance = (ms: number) =>
  act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
const region = () => screen.getByTestId("run-output-text");

beforeEach(() => {
  getMock.mockReset();
  writeText.mockReset();
  writeText.mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
});
afterEach(() => {
  vi.useRealTimers();
});

describe("OutputTab — M-O copy", () => {
  it("the approved strings, byte for byte", () => {
    expect(RUN_OUTPUT.sourceRecording).toBe("From recording");
    expect(RUN_OUTPUT.recordingRecovered).toBe(
      "Recovered from the available recording. Full output delivery could not be verified.",
    );
    expect(RUN_OUTPUT.captureGap).toBe("Some or all of this run's output could not be recovered.");
  });
});

// The packet's output-result-fixtures table, row for row.
const PARTIAL = "available output\n";
type Row = [
  name: string,
  body: Partial<RunOutput>,
  label: "sourceStdout" | "sourceRecording",
  notices: ("recordingRecovered" | "captureGap")[],
  emptySentence: "emptyFinal" | "emptyLive" | null,
];
const gapRow = { source: "stdout", complete: true, incomplete: false, capture_gap: true } as const;
const recRow = { source: "recording", complete: true, incomplete: true, capture_gap: false } as const;
const ROWS: Row[] = [
  ["gap-missing", { ...gapRow, output: "" }, "sourceStdout", ["captureGap"], null],
  ["gap-malformed", { ...gapRow, output: "" }, "sourceStdout", ["captureGap"], null],
  ["gap-uncovered", { ...gapRow, output: "" }, "sourceStdout", ["captureGap"], null],
  ["gap-partial-stdout", { ...gapRow, output: PARTIAL }, "sourceStdout", ["captureGap"], null],
  [
    "gap-partial-recording",
    { ...recRow, capture_gap: true, output: PARTIAL },
    "sourceRecording",
    ["recordingRecovered", "captureGap"],
    null,
  ],
  ["recording-partial", { ...recRow, output: PARTIAL }, "sourceRecording", ["recordingRecovered"], null],
  ["recording-empty", { ...recRow, output: "" }, "sourceRecording", ["recordingRecovered"], null],
  [
    "recording-live-empty",
    { ...recRow, complete: false, captured_at: undefined, output: "" },
    "sourceRecording",
    ["recordingRecovered"],
    null,
  ],
  ["stdout-clean-empty", { source: "stdout", output: "" }, "sourceStdout", [], "emptyFinal"],
  [
    "stdout-live-empty",
    { source: "stdout", complete: false, captured_at: undefined, output: "" },
    "sourceStdout",
    [],
    "emptyLive",
  ],
];

describe("OutputTab — M-O result fixtures", () => {
  it.each(ROWS)("%s", async (_name, body, label, notices, emptySentence) => {
    getMock.mockResolvedValue(out(body));
    const { container } = await mount({ live: body.complete === false });
    const text = container.textContent ?? "";

    // An HTTP-200 frame, never a refusal, named for the source the server gave.
    expect(screen.queryByTestId("run-output-refusal")).toBeNull();
    const pre = screen.getByRole("region", { name: RUN_OUTPUT[label] });
    expect(pre).toBe(region());
    expect(pre.getAttribute("tabindex")).toBe("0");
    const otherLabel = label === "sourceStdout" ? "sourceRecording" : "sourceStdout";
    expect(text).not.toContain(RUN_OUTPUT[otherLabel]);

    // Exactly the notices the row names, and they describe the region.
    for (const key of ["recordingRecovered", "captureGap"] as const) {
      expect(text.includes(RUN_OUTPUT[key])).toBe(notices.includes(key));
    }
    if (notices.length > 0) {
      expect(pre).toHaveAccessibleDescription(notices.map((k) => RUN_OUTPUT[k]).join(" "));
    } else {
      expect(pre.hasAttribute("aria-describedby")).toBe(false);
    }
    // A recording says the stronger sentence instead of the generic one.
    expect(text).not.toContain(RUN_OUTPUT.incomplete);

    // The body is the returned bytes; only a clean direct capture may say it printed nothing.
    const expected = body.output ?? "";
    for (const key of ["emptyFinal", "emptyLive"] as const) {
      expect(text.includes(RUN_OUTPUT[key])).toBe(emptySentence === key);
    }
    expect(pre.textContent).toBe(emptySentence ? RUN_OUTPUT[emptySentence] : expected);
    expect(text).not.toContain(RUN_OUTPUT.notCapturedDesc);
    expect(text).not.toMatch(/restart/i);

    // Copy hands over exactly the returned bytes, the empty string included.
    const copy = screen.getByRole("button", { name: RUN_OUTPUT.copyLabel });
    expect(copy).toBeEnabled();
    copy.focus();
    fireEvent.click(copy);
    await act(async () => {});
    expect(writeText).toHaveBeenCalledTimes(1);
    expect(writeText).toHaveBeenCalledWith(expected);
    expect(within(copy).getByText("Copied")).toHaveAttribute("aria-live", "polite");
    expect(document.activeElement).toBe(copy);
  });
});

describe("OutputTab — recovered from a recording", () => {
  it("final: From recording, final chip, captured time, the recovery notice, the bytes", async () => {
    getMock.mockResolvedValue(recording());
    await mount();
    expect(screen.getByText("From recording")).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.final)).toBeInTheDocument();
    expect(screen.getByText(/^Captured /)).toBeInTheDocument();
    expect(
      screen.getByText("Recovered from the available recording. Full output delivery could not be verified."),
    ).toBeInTheDocument();
    expect(screen.queryByText(RUN_OUTPUT.sourceStdout)).toBeNull();
    expect(region().textContent).toBe("$ go test ./...\nok");
    expect(region().children.length).toBe(0);
  });

  it("every true flag: recovery, mask, gap, tail-limit, in that order, and no generic incomplete line", async () => {
    getMock.mockResolvedValue(recording({ mask_scope: "globals_only", capture_gap: true, truncated: true }));
    const { container } = await mount();
    const text = container.textContent ?? "";
    const at = [
      RUN_OUTPUT.recordingRecovered,
      RUN_OUTPUT.globalsOnly,
      RUN_OUTPUT.captureGap,
      RUN_OUTPUT.truncated,
      "$ go test",
    ].map((s) => text.indexOf(s));
    expect(at.every((i) => i >= 0)).toBe(true);
    expect([...at].sort((a, b) => a - b)).toEqual(at);
    expect(text).not.toContain(RUN_OUTPUT.incomplete);
    // Only the recovery and gap notices describe the region.
    expect(region()).toHaveAccessibleDescription(`${RUN_OUTPUT.recordingRecovered} ${RUN_OUTPUT.captureGap}`);
  });

  it("a direct capture keeps its own order and the generic incomplete line", async () => {
    getMock.mockResolvedValue(out({ mask_scope: "globals_only", capture_gap: true, incomplete: true, truncated: true }));
    const { container } = await mount();
    const text = container.textContent ?? "";
    const at = [RUN_OUTPUT.globalsOnly, RUN_OUTPUT.captureGap, RUN_OUTPUT.incomplete, RUN_OUTPUT.truncated].map((s) =>
      text.indexOf(s),
    );
    expect(at.every((i) => i >= 0)).toBe(true);
    expect([...at].sort((a, b) => a - b)).toEqual(at);
    expect(text).not.toContain(RUN_OUTPUT.recordingRecovered);
    expect(region()).toHaveAccessibleDescription(RUN_OUTPUT.captureGap);
  });

  it("live: polls every 4 s, keeps focus on the same region, announces nothing, then stops at final", async () => {
    vi.useFakeTimers();
    getMock
      .mockResolvedValueOnce(recording({ complete: false, captured_at: undefined, output: "one\n" }))
      .mockResolvedValueOnce(recording({ complete: false, captured_at: undefined, output: "one\ntwo\n" }))
      .mockResolvedValue(recording({ output: "one\ntwo\nthree\n" }));
    const { container } = await mount({ live: true });
    expect(screen.getByText(RUN_OUTPUT.sourceRecording)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.live)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.recordingRecovered)).toBeInTheDocument();

    const pre = region();
    pre.focus();
    // The only live region in the frame is Copy's own, and a poll never writes to it.
    const liveRegions = () => container.querySelectorAll("[aria-live], [role='status'], [role='alert']");
    expect(liveRegions().length).toBe(1);
    const announced: string[] = [];
    const watch = new MutationObserver(() => announced.push(liveRegions()[0]?.textContent ?? ""));
    watch.observe(liveRegions()[0], { childList: true, characterData: true, subtree: true });

    await advance(4000);
    expect(getMock).toHaveBeenCalledTimes(2);
    expect(region()).toBe(pre);
    expect(pre.textContent).toBe("one\ntwo\n");
    expect(document.activeElement).toBe(pre);

    await advance(4000);
    expect(getMock).toHaveBeenCalledTimes(3);
    expect(screen.getByText(RUN_OUTPUT.final)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.recordingRecovered)).toBeInTheDocument();
    expect(region()).toBe(pre);
    expect(document.activeElement).toBe(pre);
    expect(liveRegions().length).toBe(1);
    watch.disconnect();
    expect(announced).toEqual([]);

    await advance(20_000);
    expect(getMock).toHaveBeenCalledTimes(3);
  });

  it("a gap that arrives on a poll keeps the same focused region", async () => {
    vi.useFakeTimers();
    getMock
      .mockResolvedValueOnce(out({ complete: false, captured_at: undefined, output: "one\n" }))
      .mockResolvedValue(out({ capture_gap: true, output: "one\n" }));
    await mount({ live: true });
    const pre = region();
    pre.focus();
    await advance(4000);
    expect(screen.getByText(RUN_OUTPUT.captureGap)).toBeInTheDocument();
    expect(region()).toBe(pre);
    expect(document.activeElement).toBe(pre);
  });

  it("the saving window resolves to the recovered frame", async () => {
    vi.useFakeTimers();
    getMock.mockRejectedValueOnce(refusal("run_output_not_kept", 409)).mockResolvedValue(recording());
    await mount({ endedAt: new Date(Date.now() - 10_000).toISOString() });
    expect(screen.getByText(RUN_OUTPUT.savingTitle)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.savingDesc)).toBeInTheDocument();
    await advance(4000);
    expect(screen.getByText(RUN_OUTPUT.sourceRecording)).toBeInTheDocument();
    expect(screen.queryByText(RUN_OUTPUT.savingTitle)).toBeNull();
  });

  it("the saving window resolves to a capture-gap frame, never a refusal", async () => {
    vi.useFakeTimers();
    getMock
      .mockRejectedValueOnce(refusal("run_output_not_kept", 409))
      .mockResolvedValue(out({ capture_gap: true, output: "" }));
    await mount({ endedAt: new Date(Date.now() - 10_000).toISOString() });
    expect(screen.getByText(RUN_OUTPUT.savingTitle)).toBeInTheDocument();
    await advance(4000);
    expect(screen.getByText(RUN_OUTPUT.captureGap)).toBeInTheDocument();
    expect(screen.queryByTestId("run-output-refusal")).toBeNull();
    expect(region().textContent).toBe("");
  });
});

describe("OutputTab — M-O refusals", () => {
  const noOutput = () => {
    expect(screen.getByTestId("run-output-refusal")).toBeInTheDocument();
    expect(screen.queryByTestId("run-output-text")).toBeNull();
    expect(screen.queryByRole("button", { name: RUN_OUTPUT.copyLabel })).toBeNull();
    expect(screen.queryByText("From recording")).toBeNull();
    expect(screen.queryByText(/Recovered from the available recording/)).toBeNull();
  };

  it("the owner's 410 recording_erased is the erased state, not the generic error", async () => {
    getMock.mockRejectedValue(refusal("recording_erased", 410));
    await mount();
    expect(screen.getByText("This run's output was erased")).toBeInTheDocument();
    expect(screen.getByText("It was removed at a person's erasure request and can't be restored.")).toBeInTheDocument();
    expect(screen.queryByText(RUN_COCKPIT.loadError)).toBeNull();
    expect(screen.queryByRole("button", { name: /retry/i })).toBeNull();
    noOutput();
  });

  it("an erasure that lands on a poll drops the bytes and Copy, and polling stops", async () => {
    vi.useFakeTimers();
    getMock
      .mockResolvedValueOnce(recording({ complete: false, captured_at: undefined, output: "secret-looking\n" }))
      .mockRejectedValue(refusal("recording_erased", 410));
    const { container } = await mount({ live: true });
    expect(region().textContent).toBe("secret-looking\n");
    await advance(4000);
    expect(screen.getByText(RUN_OUTPUT.erasedTitle)).toBeInTheDocument();
    expect(container.textContent).not.toContain("secret-looking");
    noOutput();
    await advance(20_000);
    expect(getMock).toHaveBeenCalledTimes(2);
  });

  // A viewer who is neither the owner nor an operator gets the answer of a run
  // that never had recording output, whatever the row holds.
  it("a refused viewer (409 run_output_not_captured) sees the existing not-captured state", async () => {
    getMock.mockRejectedValue(refusal("run_output_not_captured", 409));
    await mount();
    expect(screen.getByText(RUN_OUTPUT.notCapturedTitle)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.notCapturedDesc)).toBeInTheDocument();
    expect(screen.queryByText(RUN_OUTPUT.erasedTitle)).toBeNull();
    expect(screen.queryByText(RUN_OUTPUT.captureGap)).toBeNull();
    noOutput();
  });

  it("a refused viewer on a runner that captures stdout (409 run_output_not_kept) sees the existing not-kept state", async () => {
    getMock.mockRejectedValue(refusal("run_output_not_kept", 409));
    await mount({ endedAt: aheadByHours(-24) });
    expect(screen.getByText(RUN_OUTPUT.notKeptTitle)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.notKeptDesc)).toBeInTheDocument();
    expect(screen.queryByText(RUN_OUTPUT.erasedTitle)).toBeNull();
    noOutput();
  });
});

describe("OutputTab — failed read and Retry", () => {
  it("Retry keeps the error surface mounted and focus on Retry, sends one read, and never shows the loading line", async () => {
    vi.useFakeTimers();
    getMock.mockRejectedValueOnce(refusal("internal", 500));
    await mount();
    const surface = screen.getByTestId("run-output-refusal");
    const retry = screen.getByRole("button", { name: /retry/i });

    let fail: (e: unknown) => void = () => {};
    getMock.mockReturnValueOnce(new Promise((_res, rej) => (fail = rej)));
    retry.focus();
    fireEvent.click(retry);
    await act(async () => {});
    expect(getMock).toHaveBeenCalledTimes(2);
    expect(screen.getByTestId("run-output-refusal")).toBe(surface);
    expect(surface.getAttribute("aria-busy")).toBe("true");
    expect(screen.getByRole("button", { name: /retry/i })).toBe(retry);
    expect(document.activeElement).toBe(retry);

    // A second press while the read is in flight sends nothing more.
    fireEvent.click(retry);
    await advance(1500);
    expect(getMock).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("status")).toBeNull();
    expect(screen.queryByText(RUN_OUTPUT.loading)).toBeNull();

    // The read fails again: same surface, same button, still focused.
    await act(async () => fail(refusal("internal", 500)));
    expect(surface.getAttribute("aria-busy")).toBe("false");
    expect(screen.getByRole("button", { name: /retry/i })).toBe(retry);
    expect(document.activeElement).toBe(retry);
    expect(screen.getByText(RUN_COCKPIT.loadError)).toBeInTheDocument();

    // Once reads succeed, Retry shows the recovered frame.
    getMock.mockResolvedValue(recording());
    fireEvent.click(retry);
    await act(async () => {});
    expect(getMock).toHaveBeenCalledTimes(3);
    expect(screen.queryByTestId("run-output-refusal")).toBeNull();
    expect(screen.getByText(RUN_OUTPUT.sourceRecording)).toBeInTheDocument();
  });

  it("a 403 this console does not map falls to the same error surface, with no bytes", async () => {
    getMock.mockRejectedValue(refusal("forbidden", 403));
    await mount();
    expect(screen.getByText(RUN_COCKPIT.loadError)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
    expect(screen.queryByTestId("run-output-text")).toBeNull();
  });
});
