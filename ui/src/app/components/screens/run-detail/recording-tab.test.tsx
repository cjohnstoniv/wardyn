/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RecordingTab } from "./recording-tab";

vi.mock("../../../lib/api/audit", () => ({ audit: { listAudit: vi.fn().mockResolvedValue([]) } }));

const RUN = "11111111-1111-4111-8111-111111111111";
const BLOCKED = "This run's data_out rule keeps its content in: only a super admin can replay its recording.";
const UNREADABLE = "This run's data_out rule could not be read. Try again.";

function tab(props: Partial<React.ComponentProps<typeof RecordingTab>>) {
  return render(
    <MemoryRouter>
      <RecordingTab state="ready" recording={null} recordingDisabled={false} runId={RUN} sessions={[]} selected={RUN}
        onSelect={() => {}} onRetry={() => {}} {...props} />
    </MemoryRouter>,
  );
}

describe("RecordingTab refusals", () => {
  it("a held rule says the recording stays in, with the server's sentence as the body", () => {
    tab({ state: "error", refusal: { reason: "data_out_blocked", message: BLOCKED } });
    const box = screen.getByTestId("recording-data-out");
    expect(box).toHaveTextContent("This run's recording stays in");
    expect(box).toHaveTextContent(BLOCKED);
    expect(screen.queryByText("No recording available")).not.toBeInTheDocument();
  });

  it("an unreadable rule prints the server's sentence and Try again asks again", () => {
    const onRetry = vi.fn();
    tab({ state: "error", onRetry, refusal: { reason: "data_out_unreadable", message: UNREADABLE } });
    expect(screen.getByText(UNREADABLE)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it("a missing recording stays No recording available", () => {
    tab({ state: "ready", recording: null });
    expect(screen.getByText("No recording available")).toBeInTheDocument();
    expect(screen.queryByTestId("recording-data-out")).not.toBeInTheDocument();
  });

  it("any other failure keeps the generic load error", () => {
    tab({ state: "error" });
    expect(screen.queryByTestId("recording-data-out")).not.toBeInTheDocument();
    expect(screen.getByText("Couldn't load this run's recording.")).toBeInTheDocument();
  });
});
