/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DocumentEditor, type DocumentDiagnostic } from "./document-editor";

const YAML = "# who may reach what\nmin_confinement_class: CC2\nallowed_domains:\n  - api.example.com\n";

const seen: { value: string }[] = [];
function Editor({ initial = YAML, diagnostics = [] }: { initial?: string; diagnostics?: DocumentDiagnostic[] }) {
  const [value, setValue] = React.useState(initial);
  seen.push({ value });
  return (
    <DocumentEditor
      id="doc"
      value={value}
      format="yaml"
      onChange={setValue}
      diagnostics={diagnostics}
      label="Run document"
      describedBy="doc-help"
    />
  );
}

const box = () => screen.getByRole("textbox", { name: "Run document" }) as HTMLTextAreaElement;
const gutter = () => screen.getByTestId("document-gutter");
const last = () => seen[seen.length - 1];

describe("DocumentEditor — gutter and error marks", () => {
  it("numbers every line in an aria-hidden gutter and names the textarea", () => {
    seen.length = 0;
    render(<Editor />);
    expect(gutter()).toHaveAttribute("aria-hidden", "true");
    expect(Array.from(gutter().children).map((n) => n.textContent)).toEqual(["1", "2", "3", "4", "5"]);
    expect(box()).not.toHaveAttribute("aria-invalid");
    expect(box()).toHaveAttribute("aria-describedby", "doc-help");
    expect(gutter().querySelector("[data-error]")).toBeNull();
  });

  it("a diagnostic marks its line and sets aria-invalid", () => {
    seen.length = 0;
    render(<Editor diagnostics={[{ line: 2, column: 5, text: "Unknown field" }]} />);
    const marked = gutter().querySelectorAll("[data-error]");
    expect(marked).toHaveLength(1);
    expect(marked[0]).toHaveAttribute("data-line", "2");
    expect(box()).toHaveAttribute("aria-invalid", "true");
    expect(box()).toHaveAttribute("aria-describedby", "doc-help doc-diagnostics");
    expect(screen.getByText("Unknown field")).toBeInTheDocument();
    expect(screen.getByText("Line 2, column 5")).toBeInTheDocument();
  });

  it("sizes the box from the textarea's rows, never from the gutter", () => {
    seen.length = 0;
    render(<Editor initial={Array.from({ length: 40 }, (_, i) => `k${i}: ${i}`).join("\n")} />);
    // jsdom has no layout: the gutter must carry size containment so its one row per line
    // cannot set the row's height, and the textarea must keep the rows it was given.
    expect(gutter().className).toContain("[contain:size]");
    expect(box()).toHaveAttribute("rows", "12");
    expect(box().className).not.toContain("[contain:size]");
  });

  it("follows the textarea's scroll", () => {
    seen.length = 0;
    render(<Editor />);
    fireEvent.scroll(box(), { target: { scrollTop: 40 } });
    expect(gutter().scrollTop).toBe(40);
  });

  it("grows the gutter as lines are typed", async () => {
    seen.length = 0;
    render(<Editor initial="a: 1" />);
    expect(gutter().children).toHaveLength(1);
    await userEvent.type(box(), "{Enter}b: 2");
    expect(gutter().children).toHaveLength(2);
    expect(last().value).toBe("a: 1\nb: 2");
  });
});
