/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import type { AssistantChecks } from "../../../../lib/api/assistant";
import { fieldLabel } from "../../../../lib/change-review";
import { CHANGE_REVIEW, renderReviewSentence } from "../../../../lib/change-review-copy";
import type { ChangeReview } from "../../../../lib/types";
import { ASSISTANT as T } from "../../../wardyn/copy/assistant";
import { RAIL_CHECK } from "../../../wardyn/copy/new-run-rail";
import { ChecksStrip, checksSentence } from "./checks-strip";

afterEach(cleanup);

const SIDE = (overall: "low" | "medium" | "high", items: { field: string; value: string; risk_level: "low" | "medium" | "high"; rationale: string }[] = []) => ({ overall, items });
const WIDENS: ChangeReview = { verdict: "widens", widens: [{ field: "allowed_domains", entries: ["artifactory.example.internal"] }] };
const answer = (over: Partial<AssistantChecks> = {}): AssistantChecks => ({
  valid: true,
  errors: [],
  review: WIDENS,
  risk: { before: SIDE("low"), after: SIDE("medium", [{ field: "allowed_domains", value: "artifactory.example.internal", risk_level: "medium", rationale: "Custom egress to 1 host(s) beyond the safe baseline." }]) },
  approvals_needed: 2,
  dropped: [],
  ...over,
});

describe("ChecksStrip", () => {
  it("writes the sentence with the change review's own render function, never its own words", () => {
    const expected = renderReviewSentence({
      verdict: "widens",
      target: "Payments",
      widens: [{ field: "allowed_domains", label: fieldLabel("allowed_domains") ?? "allowed_domains", entries: ["artifactory.example.internal"] }],
      also: [],
    });
    expect(expected).toBeTruthy();
    expect(checksSentence(WIDENS, "Payments")).toBe(expected);
    render(<ChecksStrip checks={answer()} subject="Payments" kind="unit" />);
    expect(screen.getByText(expected as string)).toBeInTheDocument();
    expect(screen.getByText(CHANGE_REVIEW.VERDICT.widens)).toBeInTheDocument();
  });

  it("shows validity, what widens, the risk grade and the server's reason for it", () => {
    render(<ChecksStrip checks={answer()} subject="Payments" kind="unit" />);
    expect(screen.getByText(T.VALID)).toBeInTheDocument();
    expect(screen.getByText(T.WIDENS_DETAIL(fieldLabel("allowed_domains") ?? "", 1))).toBeInTheDocument();
    expect(screen.getByText(CHANGE_REVIEW.RISK_CHIP("Low", "Medium"))).toBeInTheDocument();
    expect(screen.getByText("Custom egress to 1 host(s) beyond the safe baseline.")).toBeInTheDocument();
  });

  it("says a unit change will be held, with the server's count", () => {
    render(<ChecksStrip checks={answer({ approvals_needed: 3 })} subject="Payments" kind="unit" />);
    expect(screen.getByText(T.HELD_FOR(3))).toBeInTheDocument();
  });

  it("says nothing about a hold for a run draft, which is never held, and nothing about a ceiling the server did not state", () => {
    // not_checked is what the server's run review always returns.
    render(<ChecksStrip checks={answer({ review: { verdict: "not_checked" } })} subject="" kind="run" />);
    expect(screen.queryByText(/held for/)).toBeNull();
    expect(screen.queryByText(T.WITHIN_CEILING)).toBeNull();
    expect(screen.getByText(T.VALID)).toBeInTheDocument();
    expect(screen.getByText(RAIL_CHECK.HINT)).toBeInTheDocument();
  });

  it("says nothing about a ceiling when the answer carries no review at all", () => {
    render(<ChecksStrip checks={answer({ review: null })} subject="" kind="run" />);
    expect(screen.queryByText(T.WITHIN_CEILING)).toBeNull();
  });

  it("says Within your ceiling only for a checked verdict that does not widen, and never while a host is left out", () => {
    render(<ChecksStrip checks={answer({ review: { verdict: "narrows_held" } })} subject="" kind="run" />);
    expect(screen.getByText(T.WITHIN_CEILING)).toBeInTheDocument();
    cleanup();
    render(<ChecksStrip checks={answer({ review: { verdict: "narrows_held" } })} subject="" kind="run" leftOut />);
    expect(screen.queryByText(T.WITHIN_CEILING)).toBeNull();
    cleanup();
    render(<ChecksStrip checks={answer({ review: { verdict: "not_checked" } })} subject="" kind="run" leftOut />);
    expect(screen.queryByText(T.WITHIN_CEILING)).toBeNull();
  });

  it("lists the errors by field and no verdict while the draft is invalid", () => {
    render(<ChecksStrip checks={answer({ valid: false, errors: [{ field: "allowed_domains", message: "not a host" }], review: null, risk: null })} subject="" kind="run" />);
    const list = screen.getByTestId("composer-check-errors");
    expect(list).toHaveTextContent("allowed_domains");
    expect(list).toHaveTextContent("not a host");
    expect(screen.queryByText(T.VALID)).toBeNull();
    expect(screen.queryByText(T.WITHIN_CEILING)).toBeNull();
  });

  it("shows every failure it is given, and none that is null", () => {
    render(<ChecksStrip checks={null} subject="" kind="run" failed={["The edit was refused.", null, "The checks failed."]} />);
    expect(screen.getAllByRole("alert").map((a) => a.textContent)).toEqual(["The edit was refused.", "The checks failed."]);
  });

  it("shows the server's sentence when the checks could not be read", () => {
    render(<ChecksStrip checks={null} subject="" kind="run" failed="The assistant is off for your organisation." />);
    expect(screen.getByRole("alert")).toHaveTextContent("The assistant is off for your organisation.");
  });
});
