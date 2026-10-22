/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The drive editor registers with the shared unsaved guard while it holds edits.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { unsavedSnapshot } from "../../../lib/unsaved-registry";
import type { UserDriveListItem } from "../../../lib/api/drives";
import { setField } from "../../../../test/set-field";
import { aheadByHours } from "../../../lib/test-clock";
import { DriveEditor } from "./drive-editor";

const EXISTING: UserDriveListItem = {
  id: "d1",
  name: "Corporate homes",
  backend: "docker_volume",
  home_template: "hash",
  reclaim: "retain",
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  grant_count: 0,
};

function renderEditor(drive: UserDriveListItem | null) {
  return render(<DriveEditor drive={drive} runnerTarget="docker" hostRootsConfigured={false} onCancel={vi.fn()} onSaved={vi.fn()} />);
}
const nameField = () => document.getElementById("drive-name")!;

afterEach(() => cleanup());

describe("DriveEditor unsaved guard", () => {
  it("a new drive: clean, dirty once typed into, clean again when the original is typed back", () => {
    renderEditor(null);
    expect(unsavedSnapshot()).toBeNull();
    setField(nameField(), "scratch");
    expect(unsavedSnapshot()).toContain("scratch");
    setField(nameField(), "");
    expect(unsavedSnapshot()).toBeNull();
  });

  it("an existing drive is compared to its own values", () => {
    renderEditor(EXISTING);
    expect(unsavedSnapshot()).toBeNull();
    setField(nameField(), "Corporate homes 2");
    expect(unsavedSnapshot()).not.toBeNull();
    setField(nameField(), "Corporate homes");
    expect(unsavedSnapshot()).toBeNull();
  });

  it("unregisters on unmount", () => {
    const { unmount } = renderEditor(null);
    setField(nameField(), "scratch");
    expect(unsavedSnapshot()).not.toBeNull();
    unmount();
    expect(unsavedSnapshot()).toBeNull();
  });
});
