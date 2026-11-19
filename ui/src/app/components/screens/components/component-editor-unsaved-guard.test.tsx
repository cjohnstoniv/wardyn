/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The component editor registers with the shared unsaved guard while it holds edits.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Component } from "../../../lib/types";
import { unsavedSnapshot } from "../../../lib/unsaved-registry";
import { COMPONENTS_ADMIN as T } from "../../wardyn/copy/components-admin";
import { setField } from "../../../../test/set-field";
import { aheadByHours } from "../../../lib/test-clock";
import { ComponentEditor } from "./component-editor";

vi.mock("../../../lib/api/components", () => ({ components: { put: vi.fn() } }));

const EXISTING: Component = {
  id: "11111111-1111-4111-8111-111111111111",
  name: "Payments API",
  version: 1,
  created_at: aheadByHours(-24),
  updated_at: aheadByHours(-24),
  definition: {
    hosts: ["api.pay.example"],
    secrets: [{ secret_name: "pay-key", delivery: { mode: "header", host: "api.pay.example" } }],
  },
};

function renderEditor(component: Component | null) {
  return render(<ComponentEditor component={component} onClose={vi.fn()} onSaved={vi.fn()} />);
}

afterEach(() => cleanup());

describe("ComponentEditor unsaved guard", () => {
  it("a new component: clean, dirty once named, clean again when the name is cleared", () => {
    renderEditor(null);
    expect(unsavedSnapshot()).toBeNull();
    setField(document.getElementById("component-name")!, "Billing");
    expect(unsavedSnapshot()).toContain("Billing");
    setField(document.getElementById("component-name")!, "");
    expect(unsavedSnapshot()).toBeNull();
  });

  it("an existing component ignores the rows' random keys, and an added then removed setting is no edit", async () => {
    const user = userEvent.setup();
    renderEditor(EXISTING);
    expect(unsavedSnapshot()).toBeNull();
    await user.click(screen.getByRole("button", { name: T.ADD_SETTING }));
    expect(unsavedSnapshot()).not.toBeNull();
    await user.click(screen.getByRole("button", { name: T.REMOVE_SETTING(1) }));
    expect(unsavedSnapshot()).toBeNull();
  });

  it("lists secret names, not values, and unregisters on unmount", () => {
    const { unmount } = renderEditor(EXISTING);
    setField(document.getElementById("component-description")!, "pays");
    expect(unsavedSnapshot()).toContain("pay-key");
    unmount();
    expect(unsavedSnapshot()).toBeNull();
  });
});

// jsdom does no layout, so this pins the classes that keep the editor inside a phone-width dialog; the e2e check in
// form-action-bars.spec.ts measures the fit.
describe("ComponentEditor at phone width", () => {
  it("the Secrets fieldset may shrink below its content and the secret row's switches stack while they do not fit", () => {
    renderEditor(EXISTING);
    const secrets = screen.getByText(T.SECRETS_LABEL, { selector: "legend" }).closest("fieldset")!;
    // A fieldset's default min-inline-size is min-content, so a no-wrap switch row would widen it past the dialog.
    expect(secrets).toHaveClass("min-w-0");
    const row = screen.getByTestId("component-secret-row");
    const tracks = new Set([...row.querySelectorAll("button[aria-pressed]")].map((b) => b.parentElement!));
    expect(tracks.size).toBe(2);
    // Below 400px by viewport, and up to ~460px by the field's own width (the switch scrolled inside its track there).
    for (const t of tracks) {
      expect(t).toHaveClass("max-[399px]:flex-col", "@max-[22rem]:flex-col");
      expect(t.parentElement).toHaveClass("@container");
    }
  });
});
