/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DurationField } from "./duration-field";

const UNITS = [
  { value: "minutes", label: "minutes" },
  { value: "hours", label: "hours" },
];

describe("DurationField", () => {
  it("is a number box beside the shared Select, never a native <select>", async () => {
    const onUnit = vi.fn();
    const onCount = vi.fn();
    const { container } = render(
      <DurationField id="d" count="3" unit="minutes" units={UNITS} onCount={onCount} onUnit={onUnit} countLabel="Lifetime" unitLabel="Lifetime unit" />,
    );
    expect(container.querySelector("select")).toBeNull();
    expect(screen.getByRole("spinbutton", { name: "Lifetime" })).toHaveValue(3);
    await userEvent.type(screen.getByRole("spinbutton", { name: "Lifetime" }), "5");
    expect(onCount).toHaveBeenLastCalledWith("35");
    await userEvent.click(screen.getByRole("combobox", { name: "Lifetime unit" }));
    await userEvent.click(await screen.findByRole("option", { name: "hours" }));
    expect(onUnit).toHaveBeenCalledWith("hours");
  });

  it("can lock the unit alone", () => {
    render(<DurationField id="d" count="" unit="hours" units={UNITS} onCount={() => {}} onUnit={() => {}} unitLabel="Unit" unitDisabled />);
    expect(screen.getByRole("combobox", { name: "Unit" })).toBeDisabled();
    expect(screen.getByRole("spinbutton")).toBeEnabled();
  });
});

// #3070: every admin dropdown is the shared Select. The tool-rule effect picker is the one documented
// native case (policy-tool-rules.tsx), so it is not listed.
describe("admin forms use the shared Select", () => {
  const MIGRATED = [
    "screens/runners/pools/pool-form-sections.tsx",
    "screens/runners/pools/pool-policy.tsx",
    "screens/settings/token-dialog.tsx",
    "screens/governance/profile-editor.tsx",
  ];
  it.each(MIGRATED)("%s has no native <select>", (file) => {
    expect(readFileSync(join(__dirname, "..", file), "utf8")).not.toMatch(/<select[\s>]/);
  });
});
