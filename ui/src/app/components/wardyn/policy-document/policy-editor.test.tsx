/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { toYaml } from "../yaml-block";
import { PolicyEditor, type PolicyEditorProps } from "./policy-editor";
import { parseSpec, type PolicySourceFormat } from "./policy-source";

const YAML = "# who may reach what\nmin_confinement_class: CC2\nallowed_domains:\n  - api.example.com # model host\n";
const VALUE = { min_confinement_class: "CC2", allowed_domains: ["api.example.com"] };

const writeText = vi.fn();
beforeEach(() => {
  writeText.mockReset().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
});

// The caller every real editor has: one source string and its format in state.
const seen: { source: string; format: PolicySourceFormat }[] = [];
function Editor({
  initial = YAML,
  initialFormat = "yaml",
  ...rest
}: { initial?: string; initialFormat?: PolicySourceFormat } & Partial<PolicyEditorProps>) {
  const [source, setSource] = React.useState(initial);
  const [format, setFormat] = React.useState<PolicySourceFormat>(initialFormat);
  seen.push({ source, format });
  const parsed = parseSpec(source, format);
  return (
    <PolicyEditor
      id="spec"
      source={source}
      format={format}
      parsed={parsed.ok ? { ok: true, value: parsed.spec } : parsed}
      onSourceChange={setSource}
      onFormatChange={(next, text) => {
        setFormat(next);
        setSource(text);
      }}
      {...rest}
    />
  );
}

const box = () => screen.getByRole("textbox") as HTMLTextAreaElement;
const last = () => seen[seen.length - 1];
beforeEach(() => {
  seen.length = 0;
});

describe("PolicyEditor — YAML by default, JSON by choice", () => {
  it("labels the source by its format and says it is being edited", () => {
    render(<Editor />);
    expect(screen.getByLabelText(/^Spec \(YAML\)/)).toBe(box());
    expect(screen.getByText("Editing")).toBeInTheDocument();
    expect(screen.getByText("Source policy")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "YAML" })).toHaveAttribute("aria-pressed", "true");
    expect(within(screen.getByRole("status")).getByText("Valid YAML")).toBeInTheDocument();
    expect(
      screen.getByText("Comments are kept while you edit; they are not stored when the policy is saved or the run launches."),
    ).toBeInTheDocument();
  });

  it("an explicit JSON source keeps the JSON label and validity words", () => {
    render(<Editor initial={JSON.stringify(VALUE, null, 2)} initialFormat="json" />);
    expect(screen.getByLabelText(/^Spec \(JSON\)/)).toBe(box());
    expect(within(screen.getByRole("status")).getByText("Valid JSON")).toBeInTheDocument();
    fireEvent.change(box(), { target: { value: "a: 1" } });
    expect(screen.getByRole("status")).toHaveTextContent(/^Invalid JSON — /);
  });

  it("Done editing is offered only where there is a read view to go back to", async () => {
    const onDone = vi.fn();
    const { unmount } = render(<Editor />);
    expect(screen.queryByRole("button", { name: "Done editing" })).toBeNull();
    unmount();
    render(<Editor onDone={onDone} />);
    await userEvent.click(screen.getByRole("button", { name: "Done editing" }));
    expect(onDone).toHaveBeenCalledTimes(1);
    // It changes presentation only: the source is what it was.
    expect(last()).toEqual({ source: YAML, format: "yaml" });
  });
});

describe("PolicyEditor — invalid source", () => {
  it("names the failure with its line and column, and describes the field by both", () => {
    render(<Editor initial={"# heading\na:\n  b: 1\n  b: 2"} />);
    expect(screen.getByRole("status")).toHaveTextContent(/^Invalid YAML — DUPLICATE_KEY/);
    expect(screen.getByText("Line 4, column 3")).toBeInTheDocument();
    expect(box()).toHaveAttribute("aria-invalid", "true");
    expect(box().getAttribute("aria-describedby")).toBe("spec-validity spec-position spec-comments");
    expect(document.getElementById("spec-position")).toHaveTextContent("Line 4, column 3");
    expect(screen.queryByText("Valid YAML")).toBeNull();
  });

  it("keeps the text exactly as typed and holds the structured controls, with a way back to the text", async () => {
    render(<Editor initial="a: [" structured={<button type="button">Minimal</button>} />);
    expect(box().value).toBe("a: [");
    expect(screen.getByRole("button", { name: "Minimal" })).toBeDisabled();
    const remedy = screen.getByRole("button", { name: "Edit policy" });
    expect(remedy).toBeEnabled();
    await userEvent.click(remedy);
    expect(box()).toHaveFocus();
    // Fixing the text clears the error and frees the controls; nothing else moved.
    fireEvent.change(box(), { target: { value: "a: []" } });
    expect(screen.getByRole("button", { name: "Minimal" })).toBeEnabled();
    expect(screen.queryByRole("button", { name: "Edit policy" })).toBeNull();
    expect(box()).not.toHaveAttribute("aria-invalid");
    expect(box().getAttribute("aria-describedby")).toBe("spec-validity spec-comments");
  });

  it("Copy source copies the exact text, invalid and commented alike", async () => {
    render(<Editor initial={"# note\na: ["} />);
    fireEvent.click(screen.getByRole("button", { name: "Copy source" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("# note\na: ["));
  });

  it("offers no conversion while the source does not parse", () => {
    render(<Editor initial="a: [" />);
    expect(screen.getByRole("button", { name: "JSON" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "YAML" })).toBeDisabled();
  });

  it("shows a refused structured edit beside the controls without calling the source invalid", () => {
    render(
      <Editor
        operationError={{ ok: false, line: 1, column: 1, message: "The edit must name a mapping field or a sequence item without gaps." }}
      />,
    );
    expect(screen.getByText(/The edit must name a mapping field/)).toHaveTextContent("Line 1, column 1");
    expect(screen.getByText("Valid YAML")).toBeInTheDocument();
    expect(box()).not.toHaveAttribute("aria-invalid");
  });
});

describe("PolicyEditor — converting the source", () => {
  it("YAML to JSON asks first, in the approved words, and Keep YAML keeps every byte", async () => {
    const user = userEvent.setup();
    render(<Editor />);
    await user.click(screen.getByRole("button", { name: "JSON" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Switch to JSON?" });
    expect(dialog).toHaveTextContent("JSON does not preserve YAML comments. Switching changes your editable source.");
    await user.click(within(dialog).getByRole("button", { name: "Keep YAML" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(box().value).toBe(YAML);
    expect(last()).toEqual({ source: YAML, format: "yaml" });
  });

  it("Switch to JSON rewrites the source as normalised JSON, without the comments", async () => {
    const user = userEvent.setup();
    render(<Editor />);
    await user.click(screen.getByRole("button", { name: "JSON" }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Switch to JSON" }));
    await waitFor(() => expect(last().format).toBe("json"));
    expect(last().source).toBe(JSON.stringify(VALUE, null, 2));
    expect(screen.getByLabelText(/^Spec \(JSON\)/)).toBe(box());
  });

  it("JSON to YAML has no comments to lose, so it converts without asking", async () => {
    const user = userEvent.setup();
    render(<Editor initial={JSON.stringify(VALUE)} initialFormat="json" />);
    await user.click(screen.getByRole("button", { name: "YAML" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(last()).toEqual({ source: `${toYaml(VALUE)}\n`, format: "yaml" });
  });

  it("typing never converts or reformats what was typed", () => {
    render(<Editor />);
    const typed = '{ "a" : 1 }   # still YAML';
    fireEvent.change(box(), { target: { value: typed } });
    expect(last()).toEqual({ source: typed, format: "yaml" });
    expect(box().value).toBe(typed);
  });
});
