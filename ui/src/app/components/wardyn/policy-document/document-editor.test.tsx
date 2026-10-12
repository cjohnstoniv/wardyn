/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PolicySourceFormat } from "../../../lib/policy-document";
import { DocumentEditor, type DocumentDiagnostic } from "./document-editor";

const YAML = "# who may reach what\nmin_confinement_class: CC2\nallowed_domains:\n  - api.example.com\n";

const seen: { value: string; format: PolicySourceFormat }[] = [];
function Editor({
  initial = YAML,
  initialFormat = "yaml",
  diagnostics = [],
  convertible = true,
}: {
  initial?: string;
  initialFormat?: PolicySourceFormat;
  diagnostics?: DocumentDiagnostic[];
  convertible?: boolean;
}) {
  const [value, setValue] = React.useState(initial);
  const [format, setFormat] = React.useState<PolicySourceFormat>(initialFormat);
  seen.push({ value, format });
  return (
    <DocumentEditor
      id="doc"
      value={value}
      format={format}
      onChange={setValue}
      onFormatChange={
        convertible
          ? (next, text) => {
              setFormat(next);
              setValue(text);
            }
          : undefined
      }
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

describe("DocumentEditor — format conversion", () => {
  it("YAML to JSON asks first, then converts what parses; Keep YAML leaves the text alone", async () => {
    seen.length = 0;
    render(<Editor />);
    await userEvent.click(screen.getByRole("button", { name: "JSON" }));
    expect(screen.getByText("Switch to JSON?")).toBeInTheDocument();
    expect(last()).toEqual({ value: YAML, format: "yaml" });

    await userEvent.click(screen.getByRole("button", { name: "Keep YAML" }));
    expect(last()).toEqual({ value: YAML, format: "yaml" });

    await userEvent.click(screen.getByRole("button", { name: "JSON" }));
    await userEvent.click(screen.getByRole("button", { name: "Switch to JSON" }));
    expect(last().format).toBe("json");
    expect(JSON.parse(last().value)).toEqual({ min_confinement_class: "CC2", allowed_domains: ["api.example.com"] });
  });

  it("JSON to YAML converts at once", async () => {
    seen.length = 0;
    render(<Editor initial={'{"min_confinement_class":"CC2"}'} initialFormat="json" />);
    await userEvent.click(screen.getByRole("button", { name: "YAML" }));
    expect(last()).toEqual({ value: "min_confinement_class: CC2\n", format: "yaml" });
  });

  it("holds the switch while the source does not parse", () => {
    seen.length = 0;
    render(<Editor initial={"a: [1,\n"} />);
    expect(screen.getByRole("button", { name: "JSON" })).toBeDisabled();
  });

  it("shows no switch when the caller does not offer conversion", () => {
    seen.length = 0;
    render(<Editor convertible={false} />);
    expect(screen.queryByRole("button", { name: "JSON" })).toBeNull();
  });
});
