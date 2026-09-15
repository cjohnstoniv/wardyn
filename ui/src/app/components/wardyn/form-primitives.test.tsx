/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// U-11: Field's hint was visual-only — no aria-describedby link from the
// control to its hint text. One pinning test for the auto id + wiring.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { Field } from "./form-primitives";

describe("Field — hint is wired to the control via aria-describedby", () => {
  it("gives the hint an id and describes the control with it", () => {
    render(
      <Field label="Name" htmlFor="name-input" hint="Helpful hint text">
        <input id="name-input" />
      </Field>,
    );
    const input = screen.getByLabelText("Name");
    const hint = screen.getByText("Helpful hint text");
    expect(hint.id).toBeTruthy();
    expect(input.getAttribute("aria-describedby")).toBe(hint.id);
  });

  it("no hint, no id, no aria-describedby — nothing invented to point at", () => {
    render(
      <Field label="Name" htmlFor="name-input">
        <input id="name-input" />
      </Field>,
    );
    expect(screen.getByLabelText("Name").getAttribute("aria-describedby")).toBeNull();
  });

  it("preserves a control's own aria-describedby alongside the hint's", () => {
    render(
      <Field label="Name" htmlFor="name-input" hint="A hint">
        <input id="name-input" aria-describedby="other-note" />
      </Field>,
    );
    const described = screen.getByLabelText("Name").getAttribute("aria-describedby") ?? "";
    // R-06 (review): also assert the hint id joined it, not just that the
    // caller's own id survived — this would stay green if the wiring itself
    // were dropped.
    expect(described.split(" ")).toEqual(expect.arrayContaining(["other-note", "name-input-hint"]));
  });
});
