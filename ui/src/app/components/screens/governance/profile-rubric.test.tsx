/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ProfileRubric standalone (0.8 #93/#96) — the three posture groups, nine
// rows, one select each. profile-editor.test.tsx already pins the round trip
// through the whole editor; this file mounts the section alone so the fold
// (empty note vs. set note, the lowest level among several set rows) doesn't
// need a saved profile or a network stub around it.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import type { AutonomyRubric } from "../../../lib/api/governance";
import { RUBRIC } from "../../../lib/governance-copy";
import { AUTONOMY_META } from "../../wardyn/autonomy-meta";
import { ProfileRubric } from "./profile-rubric";

function renderRubric(value: AutonomyRubric = {}, disabled = false) {
  const onChange = vi.fn();
  render(<ProfileRubric value={value} disabled={disabled} onChange={onChange} />);
  return { onChange };
}

describe("ProfileRubric — the three groups", () => {
  it("renders the frozen heading, intro and all nine rows across three groups", () => {
    renderRubric();
    expect(screen.getByText(RUBRIC.HEADING)).toBeInTheDocument();
    expect(screen.getByText(RUBRIC.INTRO)).toBeInTheDocument();
    expect(screen.getByText(RUBRIC.GROUP_EGRESS)).toBeInTheDocument();
    expect(screen.getByText(RUBRIC.GROUP_SECRETS)).toBeInTheDocument();
    expect(screen.getByText(RUBRIC.GROUP_BARRIER)).toBeInTheDocument();
    expect(screen.getByText(RUBRIC.GROUP_BARRIER_HINT)).toBeInTheDocument();
    for (const [label, why] of Object.values(RUBRIC.ROWS)) {
      expect(screen.getByText(label)).toBeInTheDocument();
      expect(screen.getByText(why)).toBeInTheDocument();
    }
    expect(screen.getAllByRole("combobox")).toHaveLength(9);
  });

  it("an empty rubric reads EMPTY_NOTE, and every select shows No cap", () => {
    renderRubric({});
    expect(screen.getByText(RUBRIC.EMPTY_NOTE)).toBeInTheDocument();
    for (const combo of screen.getAllByRole("combobox")) {
      expect(combo).toHaveTextContent(RUBRIC.NOCAP);
    }
  });

  it("picking a level for one row calls onChange with only that field set", async () => {
    const { onChange } = renderRubric({});
    await userEvent.click(screen.getByRole("combobox", { name: RUBRIC.ROWS.egress_sealed[0] }));
    await userEvent.click(await screen.findByRole("option", { name: AUTONOMY_META.L3.label }));
    expect(onChange).toHaveBeenCalledWith({ egress_sealed: "L3" });
  });

  it("picking No cap again clears a previously set row", async () => {
    const { onChange } = renderRubric({ secrets_none: "L3" });
    await userEvent.click(screen.getByRole("combobox", { name: RUBRIC.ROWS.secrets_none[0] }));
    await userEvent.click(await screen.findByRole("option", { name: RUBRIC.NOCAP }));
    expect(onChange).toHaveBeenCalledWith({ secrets_none: undefined });
  });

  // The fold is a MIN over every set row, not "the first row set" — pins the
  // same arithmetic governance-screen.test.tsx pins for the profiles-list chip.
  it("the footer note names the LOWEST level among several set rows, and the count", () => {
    renderRubric({ secrets_powerful: "L2", confinement_cc1: "L1", egress_open: "L3" });
    expect(screen.getByText(RUBRIC.SET_NOTE(3, AUTONOMY_META.L1.label))).toBeInTheDocument();
    expect(screen.queryByText(RUBRIC.EMPTY_NOTE)).not.toBeInTheDocument();
  });

  it("disabled disables every select", () => {
    renderRubric({}, true);
    for (const combo of screen.getAllByRole("combobox")) {
      expect(combo).toHaveAttribute("data-disabled");
    }
  });
});
