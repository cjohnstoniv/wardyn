// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { attentionFor, attentionRank, RunStateGlyph } from "./run-state-glyph";

describe("attentionFor", () => {
  it("a held approval outranks the run state", () => {
    expect(attentionFor("RUNNING", { held: true })).toBe("permission");
    expect(attentionFor("WAITING_FOR_CONFIRMATION")).toBe("permission");
  });
  it("failed and killed are evidence needing review, above working", () => {
    expect(attentionFor("FAILED")).toBe("interrupted");
    expect(attentionFor("KILLED")).toBe("interrupted");
    expect(attentionRank("interrupted")).toBeLessThan(attentionRank("working"));
  });
  it("a passive hold only matters while running", () => {
    expect(attentionFor("RUNNING", { passiveHold: true })).toBe("monitoring");
    expect(attentionFor("COMPLETED", { passiveHold: true })).toBe("done");
  });
  it("running splits into working vs active; the rest map one-to-one", () => {
    expect(attentionFor("RUNNING", { working: true })).toBe("working");
    expect(attentionFor("RUNNING")).toBe("active");
    expect(attentionFor("STARTING")).toBe("starting");
    expect(attentionFor("PENDING")).toBe("queued");
    expect(attentionFor("COMPLETED")).toBe("done");
    expect(attentionFor("STOPPED")).toBe("inactive");
    expect(attentionFor("ARCHIVED")).toBe("inactive");
  });
  it("ranks are a strict urgency order with needs-you first", () => {
    const order = ["permission", "interrupted", "monitoring", "working", "active", "queued", "done", "inactive"] as const;
    for (let i = 1; i < order.length; i++) {
      expect(attentionRank(order[i - 1])).toBeLessThanOrEqual(attentionRank(order[i]));
    }
    expect(attentionRank("permission")).toBe(1);
  });
});

describe("RunStateGlyph", () => {
  it("exposes the attention as data + accessible name", () => {
    const { container } = render(<RunStateGlyph state="RUNNING" signals={{ held: true }} />);
    const el = container.querySelector("[data-attention]");
    expect(el?.getAttribute("data-attention")).toBe("permission");
    expect(el?.getAttribute("aria-label")).toBe("Needs you");
  });
  it("never uses the accent colour for state", () => {
    for (const state of ["RUNNING", "FAILED", "KILLED", "COMPLETED", "PENDING", "STOPPED"]) {
      const { container } = render(<RunStateGlyph state={state} />);
      expect(container.innerHTML).not.toMatch(/primary/);
    }
  });
});
