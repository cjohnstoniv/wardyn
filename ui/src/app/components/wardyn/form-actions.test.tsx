/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { FormActions, SectionAnchors } from "./form-actions";

describe("FormActions", () => {
  it("says how many changes are waiting, singular and plural, and hides the count at 0", () => {
    const { rerender } = render(<FormActions changeCount={1} onCancel={() => {}} onSave={() => {}} />);
    expect(screen.getByTestId("form-actions-unsaved")).toHaveTextContent("1 unsaved change");
    expect(screen.getByTestId("form-actions-unsaved")).not.toHaveTextContent("changes");
    rerender(<FormActions changeCount={3} onCancel={() => {}} onSave={() => {}} />);
    expect(screen.getByTestId("form-actions-unsaved")).toHaveTextContent("3 unsaved changes");
    rerender(<FormActions changeCount={0} onCancel={() => {}} onSave={() => {}} />);
    expect(screen.getByTestId("form-actions-unsaved")).toBeEmptyDOMElement();
  });

  it("is pinned, wires Cancel and Save, and locks both while saving", async () => {
    const user = userEvent.setup();
    const onCancel = vi.fn();
    const onSave = vi.fn();
    const { rerender } = render(<FormActions changeCount={1} onCancel={onCancel} onSave={onSave} />);
    expect(screen.getByTestId("form-actions").className).toContain("sticky");
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(onCancel).toHaveBeenCalledTimes(1);
    expect(onSave).toHaveBeenCalledTimes(1);
    rerender(<FormActions changeCount={1} onCancel={onCancel} onSave={onSave} saving />);
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  });

  it("honours saveDisabled and a custom save label", () => {
    render(<FormActions changeCount={0} onCancel={() => {}} onSave={() => {}} saveDisabled saveLabel="Create policy" />);
    expect(screen.getByRole("button", { name: "Create policy" })).toBeDisabled();
  });
});

describe("SectionAnchors", () => {
  it("scrolls to the section instead of changing the route", async () => {
    const user = userEvent.setup();
    const scroll = vi.fn();
    render(
      <>
        <SectionAnchors items={[{ id: "sec-limits", label: "Limits" }]} />
        <div id="sec-limits" />
      </>,
    );
    document.getElementById("sec-limits")!.scrollIntoView = scroll;
    await user.click(screen.getByRole("link", { name: "Limits" }));
    expect(scroll).toHaveBeenCalledTimes(1);
  });
});
