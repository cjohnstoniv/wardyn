/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// §5c.8 — the run-detail "Effective policy" widget: nothing when the run's own
// run.create audit row is absent (or exists but never stamped clamp_warnings —
// a pre-C2 trail, "unknown" not "none tightened"), "No adjustments." when the
// row affirmatively says none, one line per tightening otherwise — read
// straight off createRequestFromAudit, never re-derived here.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import type { AuditEvent } from "../../../../lib/types";
import { AGENTS } from "../../../../lib/workspace-providers-copy";
import { EffectivePolicyWidget } from "./effective-policy";
import { RUN_WIDGETS, type WidgetContext } from "../widget-registry";
import { aheadByHours } from "../../../../lib/test-clock";

function createRow(data: Record<string, unknown> = {}): AuditEvent {
  return {
    id: "a1",
    time: aheadByHours(-1),
    actor_type: "human",
    actor: "bob@corp.example",
    action: "run.create",
    outcome: "success",
    data,
  };
}

describe("EffectivePolicyWidget", () => {
  it("renders nothing when no run.create row exists", () => {
    const { container } = render(<EffectivePolicyWidget audit={[]} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when the row exists but never stamped clamp_warnings", () => {
    // ticket: C2
    const { container } = render(<EffectivePolicyWidget audit={[createRow({})]} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders 'No adjustments.' when the row affirmatively lists none", () => {
    render(<EffectivePolicyWidget audit={[createRow({ clamp_warnings: [] })]} />);
    expect(screen.getByText(AGENTS.EFFECTIVE_TITLE)).toBeInTheDocument();
    expect(screen.getByText(AGENTS.EFFECTIVE_LEAD)).toBeInTheDocument();
    expect(screen.getByText(AGENTS.EFFECTIVE_NONE)).toBeInTheDocument();
  });

  it("renders one line per tightening, verbatim off the audit datum", () => {
    render(
      <EffectivePolicyWidget
        audit={[
          createRow({
            clamp_warnings: [
              'confinement raised from "CC1" to operator minimum "CC2"',
              "resources capped to operator maximum",
            ],
          }),
        ]}
      />,
    );
    expect(screen.getByText('confinement raised from "CC1" to operator minimum "CC2"')).toBeInTheDocument();
    expect(screen.getByText("resources capped to operator maximum")).toBeInTheDocument();
    expect(screen.queryByText(AGENTS.EFFECTIVE_NONE)).not.toBeInTheDocument();
  });

  it("ignores non-string entries the same way createRequestFromAudit does", () => {
    render(<EffectivePolicyWidget audit={[createRow({ clamp_warnings: ["ok", 42, null] })]} />);
    expect(screen.getByText("ok")).toBeInTheDocument();
  });
});

// The registry's `available` predicate must mirror the component's own gate, or
// the canvas reserves a 4x4 hole for a null tile mid-layout (the `finished`
// preset places this at y:12) and the focus-mode dock offers a button that
// opens a blank pane. Every run whose run.create row predates clamp_warnings
// is that case, which is every run recorded before C2.
describe("the registry gates the tile on the same datum the widget does", () => {
  const avail = (audit: AuditEvent[]) =>
    RUN_WIDGETS["effective-policy"].available!({ audit } as WidgetContext);

  it("no run.create row => no tile", () => {
    expect(avail([])).toBe(false);
  });

  it("a row that never stamped clamp_warnings => no tile", () => {
    // ticket: C2
    expect(avail([createRow({})])).toBe(false);
  });

  it("a row affirmatively listing none => the tile IS placed, and reads EFFECTIVE_NONE", () => {
    expect(avail([createRow({ clamp_warnings: [] })])).toBe(true);
    render(<EffectivePolicyWidget audit={[createRow({ clamp_warnings: [] })]} />);
    expect(screen.getByText(AGENTS.EFFECTIVE_NONE)).toBeInTheDocument();
  });

  it("a row with tightenings => the tile is placed", () => {
    expect(avail([createRow({ clamp_warnings: ["confinement raised"] })])).toBe(true);
  });
});
