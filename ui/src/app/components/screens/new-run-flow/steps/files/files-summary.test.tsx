/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FilesSummary, SUMMARY_DEBOUNCE_MS, summaryText, type SummaryFacts } from "./files-summary";

const BASE: SummaryFacts = { size: "4 GiB", noExtra: false, chosen: true, startsName: "payments", keptDrives: [], life: { kind: "deleted" } };

describe("summaryText", () => {
  it("joins the main drive, where it starts, the kept drives and folders as one list, and the lifecycle", () => {
    expect(summaryText(BASE)).toBe("Main drive 4 GiB · starts in payments · the main drive is deleted when the run ends");
    expect(summaryText({ ...BASE, keptDrives: ["Notes"], life: { kind: "kept", grace: "24 hours" } }))
      .toBe("Main drive 4 GiB · starts in payments · Notes is kept · the main drive is kept 24 hours after its end time");
    expect(summaryText({ ...BASE, keptDrives: ["A", "B"], life: { kind: "noEnd" } }))
      .toBe("Main drive 4 GiB · starts in payments · A and B are kept · the main drive is kept until you end the run");
    expect(summaryText({ ...BASE, keptDrives: ["A", "B", "C"] }))
      .toBe("Main drive 4 GiB · starts in payments · A, B and C are kept · the main drive is deleted when the run ends");
  });

  it("tells a kept work folder in place of the deleted main drive, and after any other lifecycle", () => {
    expect(summaryText({ ...BASE, noExtra: true, keepsWork: true }))
      .toBe("Main drive 4 GiB · starts in the main drive · the work folder is kept as a drive after the run");
    expect(summaryText({ ...BASE, keepsWork: true }))
      .toBe("Main drive 4 GiB · starts in payments · the work folder is kept as a drive after the run");
    expect(summaryText({ ...BASE, keepsWork: true, life: { kind: "kept", grace: "24 hours" } }))
      .toBe("Main drive 4 GiB · starts in payments · the main drive is kept 24 hours after its end time · the work folder is kept as a drive after the run");
    expect(summaryText({ ...BASE, noExtra: true, keepsWork: true, life: { kind: "unknown" } }))
      .toBe("Main drive 4 GiB · starts in the main drive · the work folder is kept as a drive after the run");
  });

  it("asks for a choice until something is chosen and marked as the start", () => {
    expect(summaryText({ ...BASE, chosen: false })).toBe("Add what it needs, or choose No extra files.");
    expect(summaryText({ ...BASE, startsName: undefined })).toBe("Add what it needs, or choose No extra files.");
  });

  it("states the no-extra-files choice with its lifecycle, and claims no lifecycle that is not known", () => {
    expect(summaryText({ ...BASE, noExtra: true })).toBe("Main drive 4 GiB · no extra files · the main drive is deleted when the run ends.");
    expect(summaryText({ ...BASE, noExtra: true, life: { kind: "unknown" } })).toBe("Main drive 4 GiB");
    expect(summaryText({ ...BASE, life: { kind: "unknown" } })).toBe("Main drive 4 GiB · starts in payments");
  });
});

describe("FilesSummary", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("is a polite status that changes only after the text has held for 500 ms", () => {
    const { rerender } = render(<FilesSummary text="one" />);
    const status = screen.getByRole("status");
    expect(status).toHaveAttribute("aria-live", "polite");
    expect(status).toHaveTextContent("one");
    rerender(<FilesSummary text="two" />);
    act(() => { vi.advanceTimersByTime(SUMMARY_DEBOUNCE_MS - 1); });
    expect(status).toHaveTextContent("one");
    rerender(<FilesSummary text="three" />);
    act(() => { vi.advanceTimersByTime(SUMMARY_DEBOUNCE_MS - 1); });
    expect(status).toHaveTextContent("one");
    act(() => { vi.advanceTimersByTime(1); });
    expect(status).toHaveTextContent("three");
  });
});
