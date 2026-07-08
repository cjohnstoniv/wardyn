/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ComposeQandA } from "./compose-qanda";
import type { ComposeQuestion } from "../../../lib/types";

// The CLARIFY step: render structured + free-text questions, gate Continue until
// every question is answered, and emit a ComposeQA[] transcript. Free text is
// always available as an "Other" fallback on choice questions.

const choiceQ: ComposeQuestion = {
  id: "gh",
  question: "What GitHub access?",
  why: "scope the token",
  options: ["Read-only", "Read + write"],
  multi: false,
};
const multiQ: ComposeQuestion = {
  id: "deps",
  question: "Which build deps?",
  why: "egress",
  options: ["npm", "pypi", "go"],
  multi: true,
};
const freeQ: ComposeQuestion = {
  id: "goal",
  question: "What's the goal?",
  why: "",
  options: [],
  multi: false,
};

describe("ComposeQandA", () => {
  it("walks one question at a time, gating Next/Continue, then emits the transcript", async () => {
    const onSubmit = vi.fn();
    render(
      <ComposeQandA
        questions={[choiceQ, freeQ]}
        round={0}
        submitting={false}
        onSubmit={onSubmit}
        onSkip={() => {}}
        onBack={() => {}}
      />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    // Q1 (choiceQ) shows first; Q2's field isn't mounted until we advance.
    expect(screen.getByText("What GitHub access?")).toBeInTheDocument();
    expect(screen.queryByLabelText("What's the goal?")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /next/i })).toBeDisabled();

    await user.click(screen.getByText("Read-only"));

    const next = screen.getByRole("button", { name: /next/i });
    expect(next).toBeEnabled();
    await user.click(next);

    // now on Q2 (freeQ, the last question) — the primary button reads "Continue".
    await user.type(screen.getByLabelText("What's the goal?"), "ship feature");
    await user.click(screen.getByRole("button", { name: /continue/i }));

    expect(onSubmit).toHaveBeenCalledWith([
      { question: "What GitHub access?", answer: "Read-only" },
      { question: "What's the goal?", answer: "ship feature" },
    ]);
  });

  it("combines multi-select picks into one comma-joined answer", async () => {
    const onSubmit = vi.fn();
    render(
      <ComposeQandA
        questions={[multiQ]}
        round={0}
        submitting={false}
        onSubmit={onSubmit}
        onSkip={() => {}}
        onBack={() => {}}
      />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByText("npm"));
    await user.click(screen.getByText("go"));
    await user.click(screen.getByRole("button", { name: /continue/i }));
    expect(onSubmit).toHaveBeenCalledWith([{ question: "Which build deps?", answer: "npm, go" }]);
  });

  it("offers a free-text 'Other' fallback on a choice question", async () => {
    const onSubmit = vi.fn();
    render(
      <ComposeQandA
        questions={[choiceQ]}
        round={0}
        submitting={false}
        onSubmit={onSubmit}
        onSkip={() => {}}
        onBack={() => {}}
      />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByText("Other"));
    await user.type(screen.getByPlaceholderText("Describe…"), "admin only");
    await user.click(screen.getByRole("button", { name: /continue/i }));
    expect(onSubmit).toHaveBeenCalledWith([{ question: "What GitHub access?", answer: "admin only" }]);
  });

  it("Skip & propose anyway and Back are wired", async () => {
    const onSkip = vi.fn();
    const onBack = vi.fn();
    render(
      <ComposeQandA
        questions={[freeQ]}
        round={0}
        submitting={false}
        onSubmit={() => {}}
        onSkip={onSkip}
        onBack={onBack}
      />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: /skip & propose anyway/i }));
    expect(onSkip).toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: /back/i }));
    expect(onBack).toHaveBeenCalled();
  });
});
