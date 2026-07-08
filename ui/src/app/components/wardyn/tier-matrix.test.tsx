/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { TierMatrix } from "./tier-matrix";
import {
  CC_MATRIX_ROWS,
  CC_MATRIX_WHERE,
  CC_META,
  CC_ORDER,
  CONFINEMENT_CONSTANT_NOTE,
} from "./cc-meta";
import { RESIDUAL_PREFIX } from "./copy";

// TierMatrix is a rendering of CC_MATRIX_ROWS, so this file is BOTH the drift
// guard (every row label + every graded cell must render, from the data) AND the
// honesty guard: no wire code (CC1/2/3) or substrate mechanism (gVisor/runc/Kata)
// ever reaches VISIBLE copy — they live only in tooltips/sr-only, because this
// dialog opens inside the New Run dialog which asserts exactly that (wizard.spec.ts).
describe("TierMatrix", () => {
  it("renders every protection row from CC_MATRIX_ROWS + the friendly tier headers", () => {
    render(<TierMatrix />);
    for (const row of CC_MATRIX_ROWS) {
      expect(screen.getByText(row.label)).toBeInTheDocument();
    }
    // Columns use the friendly labels (Fence/Wall/Vault), never the wire code.
    for (const cc of CC_ORDER) {
      expect(screen.getAllByText(CC_META[cc].label).length).toBeGreaterThan(0);
    }
  });

  it("grades every cell with the matching three-state tone (drift guard vs the data)", () => {
    const { container } = render(<TierMatrix />);
    const want = { yes: 0, caveat: 0, no: 0 };
    for (const row of CC_MATRIX_ROWS) {
      for (const cc of CC_ORDER) want[row.cells[cc]]++;
    }
    // The success/warning/danger tones come ONLY from the mark cells here, so the
    // counts must match the data exactly.
    const got = {
      yes: container.querySelectorAll(".text-success").length,
      caveat: container.querySelectorAll(".text-warning").length,
      no: container.querySelectorAll(".text-danger").length,
    };
    expect(got).toEqual(want);
  });

  it("caveat cells carry the verbatim residual-risk (RESIDUAL_PREFIX + doesntProtect) as sr-only text", () => {
    render(<TierMatrix />);
    for (const row of CC_MATRIX_ROWS) {
      for (const cc of CC_ORDER) {
        if (row.cells[cc] !== "caveat") continue;
        const text = `${RESIDUAL_PREFIX} ${CC_META[cc].doesntProtect}`;
        expect(screen.getAllByText(text).length).toBeGreaterThan(0);
      }
    }
  });

  it("shows the where-it-runs row and the every-tier constant note verbatim", () => {
    render(<TierMatrix />);
    expect(screen.getByText(CC_MATRIX_WHERE.label)).toBeInTheDocument();
    for (const cc of CC_ORDER) {
      expect(screen.getByText(CC_MATRIX_WHERE.cells[cc])).toBeInTheDocument();
    }
    expect(screen.getByText(CONFINEMENT_CONSTANT_NOTE)).toBeInTheDocument();
  });

  it("never exposes a wire code or substrate mechanism as visible copy", () => {
    render(<TierMatrix />);
    // getByText matches visible text content, not title/aria attributes — so the
    // mechanism living in the ConfinementChip tooltip is fine, visible copy is not.
    expect(screen.queryAllByText(/\bCC[123]\b/)).toHaveLength(0);
    expect(screen.queryAllByText(/gVisor/i)).toHaveLength(0);
    expect(screen.queryAllByText(/runc/i)).toHaveLength(0);
    expect(screen.queryAllByText(/\bKata\b/i)).toHaveLength(0);
  });
});
