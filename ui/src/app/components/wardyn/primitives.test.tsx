/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import {
  RunStateBadge,
  ConfinementChip,
  ActorTypeChip,
  OutcomeBadge,
  ApprovalKindChip,
  ApprovalStateBadge,
} from "./primitives";
import type {
  RunState,
  ConfinementClass,
  ActorType,
  Outcome,
  ApprovalKind,
  ApprovalState,
} from "../../lib/types";

// Regression for the COMPLETED-state cluster + the "enum badges crash on
// unmapped wire values" HIGH finding. Before the fix, a backend value the UI
// did not know about (e.g. COMPLETED) made `meta[value]` undefined and
// dereferencing it threw inside render, blanking the whole console. These tests
// assert (1) COMPLETED renders, and (2) every enum badge degrades to a neutral
// chip showing the raw value instead of throwing.

describe("RunStateBadge", () => {
  it("renders the COMPLETED state", () => {
    render(<RunStateBadge state="COMPLETED" />);
    expect(screen.getByText("Completed")).toBeInTheDocument();
  });

  it("renders a known state", () => {
    render(<RunStateBadge state="RUNNING" />);
    expect(screen.getByText("Running")).toBeInTheDocument();
  });

  // CONSOLE-RULES §5: solid saturated red is KILLED's reserved treatment and it
  // does not survive as bare text, so the label variant — which drops the pill
  // everywhere else, for the board card's row 2 — keeps the chip for this one
  // state. Nothing pinned that carve-out.
  it("the label variant drops the pill, except for KILLED", () => {
    const killed = render(<RunStateBadge state="KILLED" variant="label" />);
    expect(screen.getByText("Killed")).toBeInTheDocument();
    expect(killed.container.querySelector(".bg-danger")).not.toBeNull();
    killed.unmount();

    // FAILED is danger too, but not solid — it renders as the bare word.
    const failed = render(<RunStateBadge state="FAILED" variant="label" />);
    expect(screen.getByText("Failed")).toBeInTheDocument();
    expect(failed.container.querySelector(".bg-danger")).toBeNull();
  });

  it("does not throw on an unknown state and shows the raw value", () => {
    const unknown = "SOME_FUTURE_STATE" as RunState;
    expect(() => render(<RunStateBadge state={unknown} />)).not.toThrow();
    expect(screen.getByText("SOME_FUTURE_STATE")).toBeInTheDocument();
  });
});

describe("fail-soft enum badges", () => {
  it("ConfinementChip tolerates an unknown class", () => {
    expect(() =>
      render(<ConfinementChip value={"CC9" as ConfinementClass} />),
    ).not.toThrow();
    expect(screen.getByText("CC9")).toBeInTheDocument();
  });

  it("ActorTypeChip tolerates an unknown actor", () => {
    expect(() => render(<ActorTypeChip type={"robot" as ActorType} />)).not.toThrow();
    expect(screen.getByText("robot")).toBeInTheDocument();
  });

  it("OutcomeBadge tolerates an unknown outcome", () => {
    expect(() => render(<OutcomeBadge outcome={"weird" as Outcome} />)).not.toThrow();
    expect(screen.getByText("weird")).toBeInTheDocument();
  });

  it("ApprovalKindChip tolerates an unknown kind", () => {
    expect(() =>
      render(<ApprovalKindChip kind={"mystery" as ApprovalKind} />),
    ).not.toThrow();
    expect(screen.getByText("mystery")).toBeInTheDocument();
  });

  it("ApprovalStateBadge tolerates an unknown state", () => {
    expect(() =>
      render(<ApprovalStateBadge state={"PARTIAL" as ApprovalState} />),
    ).not.toThrow();
    // Title-cased rendering of the raw value.
    expect(screen.getByText("Partial")).toBeInTheDocument();
  });
});

// Plan section G: ConfinementChip's tooltip used to fabricate security
// semantics ("Permissive sandbox" / "Scoped credentials + egress filtering" /
// "Hardened: HITL approvals required") that the backend does not tie to the
// confinement class. The hint now comes from the shared, honest
// wardyn/cc-meta.ts (same source step-confinement.tsx reads) — substrate
// only. These tests pin the honest wording and guard against the fabricated
// strings creeping back in.
describe("ConfinementChip tooltip honesty", () => {
  function titleOf(value: ConfinementClass): string {
    const { container } = render(<ConfinementChip value={value} />);
    return container.querySelector("[title]")?.getAttribute("title") ?? "";
  }

  it("CC1 tooltip describes the hardened-runc substrate", () => {
    expect(titleOf("CC1")).toMatch(/runc/i);
  });

  it("CC2 tooltip describes the gVisor substrate", () => {
    expect(titleOf("CC2")).toMatch(/gVisor/i);
  });

  it("CC3 tooltip describes the Kata microVM substrate", () => {
    expect(titleOf("CC3")).toMatch(/kata microVM/i);
  });

  it("no class's tooltip fabricates credential/egress/HITL semantics", () => {
    for (const cc of ["CC1", "CC2", "CC3"] as ConfinementClass[]) {
      const title = titleOf(cc);
      expect(title).not.toMatch(/scoped credentials/i);
      expect(title).not.toMatch(/hitl/i);
      expect(title).not.toMatch(/permissive sandbox/i);
    }
  });

  it("keeps the internal wire class in the tooltip ONLY, never in accessible content (D4)", () => {
    const { container } = render(<ConfinementChip value="CC2" />);
    // The tooltip carries the mechanism + internal class for a sighted power-user's hover…
    const title = container.querySelector("[title]")?.getAttribute("title") ?? "";
    expect(title).toMatch(/CC2/);
    // …but the wire code must NEVER reach accessible content — no sr-only twin,
    // no aria-label leak. A screen reader reads the DOM text (the visible label),
    // and getByText / textContent must not surface the internal class or mechanism.
    expect(container.textContent).not.toMatch(/\bCC[123]\b/);
    expect(container.textContent).not.toMatch(/gVisor|runc|Kata/i);
    // The visible barrier label IS the accessible name.
    expect(screen.getByText(/Fence|Wall|Vault/)).toBeInTheDocument();
  });
});

// ui-approvals-2: ApprovalKindChip used to keep its OWN wire-shaped label
// table ("egress-domain", "tool-call") instead of reading copy.ts's
// APPROVAL_KIND_LABEL (via WIRE_TO_COPY) the way approvals.tsx's adjacent
// title/banner already does — so the chip and the human-language title next
// to it disagreed on every row.
describe("ApprovalKindChip — human copy, not a raw wire token", () => {
  it("renders the SAME human labels APPROVAL_KIND_LABEL defines, not the wire enum re-punctuated", () => {
    render(<ApprovalKindChip kind="egress_domain" />);
    expect(screen.getByText("Network egress")).toBeInTheDocument();
    expect(screen.queryByText("egress-domain")).not.toBeInTheDocument();
  });

  it("credential -> 'Credential'", () => {
    render(<ApprovalKindChip kind="credential" />);
    expect(screen.getByText("Credential")).toBeInTheDocument();
  });

  it("tool_call -> 'Tool call', not 'tool-call'", () => {
    render(<ApprovalKindChip kind="tool_call" />);
    expect(screen.getByText("Tool call")).toBeInTheDocument();
    expect(screen.queryByText("tool-call")).not.toBeInTheDocument();
  });
});
