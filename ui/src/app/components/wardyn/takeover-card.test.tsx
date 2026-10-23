/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { ApprovalRequest } from "../../lib/types";
import { TakeoverCard } from "./takeover-card";

// #3031: a PENDING takeover card does not wear the name of its tab as a chip.
describe("TakeoverCard", () => {
  it("renders no Pending state chip on a PENDING card", () => {
    const item = {
      id: "apr_t1",
      run_id: "run_1",
      kind: "human_takeover",
      requested_scope: { reason: "The agent is stuck on a sign-in prompt." },
      state: "PENDING",
      requested_at: new Date().toISOString(),
    } as ApprovalRequest;
    render(<TakeoverCard item={item} busy={false} onTakeOver={vi.fn()} onDecline={vi.fn()} />);
    const card = screen.getByTestId("takeover-card");
    expect(within(card).getByText("The agent is stuck on a sign-in prompt.")).toBeInTheDocument();
    expect(within(card).queryByText("Pending")).not.toBeInTheDocument();
  });
});
