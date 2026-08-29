/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AgentRun, RunState } from "../../../lib/types";
import { RunCard } from "./run-card";
import { approvalSignals, type RunSignals } from "./board-groups";

const run = (over: Partial<AgentRun> = {}): AgentRun => ({
  id: "run_3b7f10c4aa99",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/payments-api",
  task: "Rotate the staging credentials",
  confinement_class: "CC3",
  state: "RUNNING" as RunState,
  spiffe_id: "spiffe://x",
  runner_target: "docker",
  ...over,
});

function renderCard(r: AgentRun, signals: RunSignals = new Map()) {
  return render(
    <MemoryRouter>
      <RunCard run={r} signals={signals} onOpen={vi.fn()} onKill={vi.fn()} />
    </MemoryRouter>,
  );
}

// CONSOLE-RULES §5: every actor on a row is two adjacent glyphs, never fused —
// WHO (the agent monogram) and WHAT (the state). The state's WORD moved to
// row 2, but it stays in the DOM: the e2e suite reads state off that text.
describe("RunCard — two-row anatomy", () => {
  it("carries WHO and WHAT as separate glyphs, and the state word on row 2", () => {
    renderCard(run({ state: "WAITING_FOR_CONFIRMATION" }));
    // WHO — the monogram, not fused into the state.
    expect(screen.getByText("CC")).toBeInTheDocument();
    // WHAT — its own labelled glyph.
    expect(screen.getByRole("img", { name: "Needs you" })).toBeInTheDocument();
    // …and the word the e2e suite asserts.
    expect(screen.getByText("Awaiting confirmation")).toBeInTheDocument();
  });

  it("row 2 carries repo, barrier, short id and age", () => {
    renderCard(run());
    expect(screen.getByText("acme/payments-api")).toBeInTheDocument();
    expect(screen.getByText("Vault")).toBeInTheDocument();
    // run_ prefix stripped, clipped with an ellipsis, full id on the title.
    expect(screen.getByTitle("run_3b7f10c4aa99")).toHaveTextContent("3b7f10c4…");
  });

  it("a held approval says what is waiting; a failure adds nothing beyond the glyph and Review", () => {
    const held = approvalSignals([
      {
        id: "a1",
        run_id: "run_3b7f10c4aa99",
        kind: "egress_domain",
        requested_scope: { host: "h", mode: "wait_for_review" },
        state: "PENDING",
        requested_at: new Date().toISOString(),
      },
    ]);
    renderCard(run(), held);
    expect(screen.getByText("1 waiting · sandbox held")).toBeInTheDocument();

    // The sentence that used to restate the state on every attention card is
    // gone — the glyph and the Review button carry it.
    renderCard(run({ id: "run-2", state: "FAILED" }));
    expect(screen.queryByText(/Run failed — review what happened/)).toBeNull();
    expect(screen.queryByText(/Waiting for your confirmation/)).toBeNull();
  });

  it("Review is always reachable on a card that needs eyes; Attach is revealed, never hover-only", () => {
    const { unmount } = renderCard(run({ state: "FAILED" }));
    expect(screen.getByRole("button", { name: "Review" })).toBeInTheDocument();
    unmount();

    renderCard(run({ interactive: true, state: "RUNNING" }));
    const attach = screen.getByRole("button", { name: /Attach/ });
    // Revealed by hover AND focus-within, so it is reachable from the keyboard.
    expect(attach.className).toContain("group-hover:opacity-100");
    expect(attach.className).toContain("group-focus-within:opacity-100");
  });

  it("the barrier strength strip is gone from the card — the chip already names the tier", () => {
    const { container } = renderCard(run());
    // The strip renders its ladder as a row of filled segments; only the chip
    // and its icon should carry the tier here now.
    expect(screen.getAllByText("Vault")).toHaveLength(1);
    expect(container.querySelectorAll(".bg-vault-fg")).toHaveLength(0);
  });
});
