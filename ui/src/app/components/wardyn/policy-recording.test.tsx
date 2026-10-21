/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, within, fireEvent } from "@testing-library/react";
import type { RunPolicySpec } from "../../lib/types";

const siteConfig = vi.fn();
const healthRead = vi.fn();
vi.mock("../../lib/api/health", () => ({
  health: { getSiteConfig: () => siteConfig(), health: () => healthRead() },
}));

import { RecordingSection } from "./policy-recording";

const spec = (recording?: RunPolicySpec["recording"]): RunPolicySpec =>
  ({ allowed_domains: [], min_confinement_class: "CC1", recording }) as unknown as RunPolicySpec;

describe("RecordingSection (#2968, M7 Knobs-B and States)", () => {
  beforeEach(() => {
    siteConfig.mockResolvedValue({});
    healthRead.mockResolvedValue({ components: { recording: { selected: "pg" } } });
  });

  it("writes the chosen level and shows its board description; keystrokes are a line, not a control", () => {
    const onSpecChange = vi.fn();
    render(<RecordingSection spec={spec("terminal")} onSpecChange={onSpecChange} />);
    const group = screen.getByRole("radiogroup", { name: "Recording" });
    expect(within(group).getByRole("radio", { name: "Terminal" })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByText("Records terminal sessions only. No screen.")).toBeInTheDocument();
    fireEvent.click(within(group).getByRole("radio", { name: "Screen and actions" }));
    expect(onSpecChange).toHaveBeenCalledWith(expect.objectContaining({ recording: "screen_actions" }));
    const keys = screen.getByTestId("policy-recording-keystrokes");
    expect(keys).toHaveTextContent("KeystrokesNever recordedNot a setting. No wall, team or run can turn it on.");
    expect(within(keys).queryByRole("radio")).toBeNull();
    expect(within(group).queryByRole("radio", { name: /keystroke/i })).toBeNull();
  });

  it("a locked organisation minimum strikes out the lower levels and says why", async () => {
    siteConfig.mockResolvedValue({ recording: { level: "terminal", locked: true } });
    render(<RecordingSection spec={spec()} onSpecChange={vi.fn()} />);
    expect(await screen.findByTestId("policy-recording-floor")).toHaveTextContent(
      "Your organisation requires at least Terminal. Off isn't available here.",
    );
    const group = screen.getByRole("radiogroup", { name: "Recording" });
    expect(within(group).getByRole("radio", { name: "Off" })).toBeDisabled();
    expect(within(group).getByRole("radio", { name: "Terminal" })).toBeEnabled();
    expect(screen.getByText("Not set: the organisation's level applies.")).toBeInTheDocument();
  });

  it("with no recording store it says so in the Recording tab's words", async () => {
    healthRead.mockResolvedValue({ components: { recording: { selected: "none" } } });
    render(<RecordingSection spec={spec()} onSpecChange={vi.fn()} />);
    expect(await screen.findByText("Session recording is disabled on this deployment")).toBeInTheDocument();
    expect(screen.getByText("Disabled")).toBeInTheDocument();
  });
});
