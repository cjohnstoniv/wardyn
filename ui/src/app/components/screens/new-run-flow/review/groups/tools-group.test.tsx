/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { initialDraft, type RunIntent } from "../../draft/run-intent";
import { ToolsGroup } from "./tools-group";

afterEach(cleanup);

const INTENT: RunIntent = {
  shape: "agent_task",
  experience: "interactive",
  tools: [{ id: "claude-code", kind: "harness" }, { id: "codex-cli", kind: "harness" }],
  startup: { kind: "harness", tool: "claude-code" },
};

// A harness tile's name and status chip need about 250 px; a 320 px phone has about 150, so the chip ran
// past the group card and was clipped. Under the console's narrow breakpoint the tile stacks.
describe("Review: a harness tile on a narrow screen", () => {
  it("stacks its name, status chip and prompt line under the narrow breakpoint", () => {
    const draft = { ...initialDraft("alice"), intent: INTENT };
    render(<ToolsGroup draft={draft} pre={null} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />);
    const tile = screen.getByText(NR_REVIEW.HARNESS_AFTER).closest("div")!;
    expect(tile.className).toContain("max-[520px]:grid-cols-1");
    expect(tile.className).toContain("max-[520px]:justify-items-start");
  });
});
