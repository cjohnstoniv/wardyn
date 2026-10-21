/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The user-type editor registers with the shared unsaved guard while it holds edits.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { unsavedSnapshot } from "../../../lib/unsaved-registry";
import type { UserType } from "../../../lib/types";
import { setField } from "../../../../test/set-field";
import { aheadByHours } from "../../../lib/test-clock";
import { UserTypeEditor } from "./user-type-editor";

const EXISTING: UserType = {
  id: "contractor",
  name: "Contractor",
  description: "Outside engineers",
  priority: 10,
  built_in: false,
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
};

function renderEditor(type: UserType | null) {
  return render(<UserTypeEditor type={type} disabled={false} governance={null} onCancel={vi.fn()} onSaved={vi.fn()} onSubmitted={vi.fn()} />);
}
const nameField = () => document.getElementById("user-type-name")!;

afterEach(() => cleanup());

describe("UserTypeEditor unsaved guard", () => {
  it("a new type: clean, dirty once typed into, clean again when the original is typed back", () => {
    renderEditor(null);
    expect(unsavedSnapshot()).toBeNull();
    setField(nameField(), "Intern");
    expect(unsavedSnapshot()).toContain("Intern");
    setField(nameField(), "");
    expect(unsavedSnapshot()).toBeNull();
  });

  it("an existing type is compared to its own values", () => {
    renderEditor(EXISTING);
    expect(unsavedSnapshot()).toBeNull();
    setField(document.getElementById("user-type-priority")!, "20");
    expect(unsavedSnapshot()).not.toBeNull();
    setField(document.getElementById("user-type-priority")!, "10");
    expect(unsavedSnapshot()).toBeNull();
  });

  it("unregisters on unmount", () => {
    const { unmount } = renderEditor(null);
    setField(nameField(), "Intern");
    expect(unsavedSnapshot()).not.toBeNull();
    unmount();
    expect(unsavedSnapshot()).toBeNull();
  });
});
