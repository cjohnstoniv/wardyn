/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The policy editor's Data out section: the rule's three values, what each does, and the floor a member's
// own ceiling already sets. The sentences are written out here, so a drift in copy/data-out.ts fails this
// suite.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { RunPolicySpec } from "../../lib/types";

const getDefaultPolicy = vi.fn();
vi.mock("../../lib/api/policies", () => ({
  policies: { getDefaultPolicy: (...a: unknown[]) => getDefaultPolicy(...a) },
}));

import { DataOutSection } from "./policy-data-out";

const spec = (data_out?: RunPolicySpec["data_out"]): RunPolicySpec =>
  ({ allowed_domains: [], min_confinement_class: "CC1", ...(data_out ? { data_out } : {}) }) as unknown as RunPolicySpec;

const NOT_COVERED = "Reading the terminal or the screen is not covered: turn off interactive access for that.";
const UNSET = "Not set: allowed, unless a layer above holds content in.";
const ALLOWED = "Content can leave the run by every path its other rules allow.";
const REVIEW =
  "Files leave one at a time through the Files panel, each with a person's yes. SSH file transfer, SSH commands, port forwards, run output, recording replay, web apps and computer folders are off.";
const BLOCKED_RUN = "Nothing leaves by those paths, the Files panel included, and git push is off.";
const BLOCKED_GOVERNED = "Nothing leaves by those paths, the Files panel included; git push, drives and keeping the machine are off.";

const group = () => screen.getByRole("radiogroup", { name: "Data out" });
const radio = (name: string) => within(group()).getByRole("radio", { name });

beforeEach(() => {
  getDefaultPolicy.mockReset().mockResolvedValue({ allowed_domains: [] });
});

describe("DataOutSection, the section", () => {
  it("not set: says so, checks nothing, and writes nothing until a value is picked", () => {
    const onSpecChange = vi.fn();
    render(<DataOutSection spec={spec()} onSpecChange={onSpecChange} />);
    expect(screen.getByText(UNSET)).toBeInTheDocument();
    for (const name of ["Allowed", "Review", "Blocked"]) expect(radio(name)).toHaveAttribute("aria-checked", "false");
    expect(onSpecChange).not.toHaveBeenCalled();
    fireEvent.click(radio("Review"));
    expect(onSpecChange).toHaveBeenCalledTimes(1);
    expect(onSpecChange.mock.calls[0][0]).toMatchObject({ data_out: "review", min_confinement_class: "CC1" });
  });

  it("each value reads its own line", () => {
    const { rerender } = render(<DataOutSection spec={spec("allowed")} onSpecChange={vi.fn()} />);
    expect(radio("Allowed")).toHaveAttribute("aria-checked", "true");
    expect(screen.getByText(ALLOWED)).toBeInTheDocument();
    rerender(<DataOutSection spec={spec("review")} onSpecChange={vi.fn()} />);
    expect(radio("Review")).toHaveAttribute("aria-checked", "true");
    expect(screen.getByText(REVIEW)).toBeInTheDocument();
    rerender(<DataOutSection spec={spec("blocked")} onSpecChange={vi.fn()} />);
    expect(radio("Blocked")).toHaveAttribute("aria-checked", "true");
    expect(screen.getByText(BLOCKED_RUN)).toBeInTheDocument();
  });

  it("blocked in a profile's ceiling also says drives and kept machines are off; in a run policy only push", () => {
    const { rerender } = render(<DataOutSection spec={spec("blocked")} onSpecChange={vi.fn()} ceiling />);
    expect(screen.getByText(BLOCKED_GOVERNED)).toBeInTheDocument();
    expect(screen.queryByText(BLOCKED_RUN)).toBeNull();
    rerender(<DataOutSection spec={spec("blocked")} onSpecChange={vi.fn()} />);
    expect(screen.getByText(BLOCKED_RUN)).toBeInTheDocument();
    expect(screen.queryByText(BLOCKED_GOVERNED)).toBeNull();
  });

  it("says the terminal and the screen are not covered, for every value and for none", () => {
    const { rerender } = render(<DataOutSection spec={spec()} onSpecChange={vi.fn()} />);
    expect(screen.getByText(NOT_COVERED)).toBeInTheDocument();
    for (const v of ["allowed", "review", "blocked"] as const) {
      rerender(<DataOutSection spec={spec(v)} onSpecChange={vi.fn()} />);
      expect(screen.getByText(NOT_COVERED)).toBeInTheDocument();
    }
  });
});

describe("DataOutSection, the floor a member's own ceiling sets", () => {
  it("a review floor strikes out and disables Allowed, shows the chip and names the unit", async () => {
    getDefaultPolicy.mockResolvedValue({ allowed_domains: [], data_out: "review", unit_name: "Payments" });
    render(<DataOutSection spec={spec("review")} onSpecChange={vi.fn()} />);
    expect(await screen.findByText("Set above")).toBeInTheDocument();
    expect(screen.getByTestId("policy-data-out-floor")).toHaveTextContent("Payments holds this at Review or stricter, so Allowed is not available.");
    expect(radio("Allowed")).toBeDisabled();
    expect(radio("Allowed").className).toContain("line-through");
    expect(radio("Review")).toBeEnabled();
    expect(radio("Blocked")).toBeEnabled();
  });

  it("a blocked floor strikes out both looser values and joins them with 'and'", async () => {
    getDefaultPolicy.mockResolvedValue({ allowed_domains: [], data_out: "blocked", unit_name: "Payments" });
    render(<DataOutSection spec={spec()} onSpecChange={vi.fn()} />);
    expect(await screen.findByTestId("policy-data-out-floor")).toHaveTextContent(
      "Payments holds this at Blocked or stricter, so Allowed and Review is not available.",
    );
    expect(radio("Allowed")).toBeDisabled();
    expect(radio("Review")).toBeDisabled();
    expect(radio("Blocked")).toBeEnabled();
  });

  it("a failed read strikes nothing out", async () => {
    getDefaultPolicy.mockRejectedValue(new Error("boom"));
    render(<DataOutSection spec={spec()} onSpecChange={vi.fn()} />);
    await waitFor(() => expect(getDefaultPolicy).toHaveBeenCalledTimes(1));
    for (const name of ["Allowed", "Review", "Blocked"]) expect(radio(name)).toBeEnabled();
    expect(screen.queryByText("Set above")).toBeNull();
    expect(screen.queryByTestId("policy-data-out-floor")).toBeNull();
  });

  it("a floor with no unit name keeps the chip and the strike-out but draws no line", async () => {
    getDefaultPolicy.mockResolvedValue({ allowed_domains: [], data_out: "review" });
    render(<DataOutSection spec={spec()} onSpecChange={vi.fn()} />);
    expect(await screen.findByText("Set above")).toBeInTheDocument();
    expect(radio("Allowed")).toBeDisabled();
    expect(screen.queryByTestId("policy-data-out-floor")).toBeNull();
  });

  it("an allowed or absent floor strikes nothing", async () => {
    getDefaultPolicy.mockResolvedValue({ allowed_domains: [], data_out: "allowed", unit_name: "Payments" });
    render(<DataOutSection spec={spec()} onSpecChange={vi.fn()} />);
    await waitFor(() => expect(getDefaultPolicy).toHaveBeenCalledTimes(1));
    expect(radio("Allowed")).toBeEnabled();
    expect(screen.queryByText("Set above")).toBeNull();
  });

  it("a profile's ceiling reads no floor", () => {
    render(<DataOutSection spec={spec()} onSpecChange={vi.fn()} ceiling />);
    expect(getDefaultPolicy).not.toHaveBeenCalled();
    expect(screen.queryByText("Set above")).toBeNull();
  });
});
