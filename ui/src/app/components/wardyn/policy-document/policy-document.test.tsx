/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { RunPolicySpec } from "../../../lib/types";
import { toYaml } from "../yaml-block";
import { PolicyDocument, PolicyDocumentView, type PolicyDocumentProps, type PolicyView } from "./policy-document";
import { parseSpec, type PolicySourceFormat } from "./policy-source";

const SPEC = { min_confinement_class: "CC2", allowed_domains: ["api.example.com"] } as unknown as RunPolicySpec;
const YAML = "# who may reach what\nmin_confinement_class: CC2\nallowed_domains:\n  - api.example.com # model host\n";
const JSON_TEXT = '{"min_confinement_class":"CC2",\n  "allowed_domains":["api.example.com"]}';

const writeText = vi.fn();
beforeEach(() => {
  writeText.mockReset().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
});

// The wrapper every real consumer is: it owns the view, and parses its own text.
function Doc({
  text,
  format = "yaml",
  ...rest
}: { text?: string; format?: PolicySourceFormat } & Partial<PolicyDocumentProps>) {
  const [view, setView] = React.useState<PolicyView>("summary");
  const parsed = text === undefined ? null : parseSpec(text, format);
  return (
    <PolicyDocument
      spec={parsed ? (parsed.ok ? parsed.spec : null) : SPEC}
      source={text === undefined ? undefined : { text, format }}
      view={view}
      onViewChange={setView}
      {...rest}
    />
  );
}

const press = (name: string) => fireEvent.click(screen.getByRole("button", { name }));
const blockText = () => Array.from(document.querySelectorAll("pre code > div"), (d) => d.textContent).join("\n");

async function copied(name: string): Promise<string> {
  writeText.mockClear();
  press(name);
  await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
  return writeText.mock.calls[0][0] as string;
}

describe("PolicyDocument — three views of one policy", () => {
  it("offers Summary, YAML and JSON, and shows the one it is given", () => {
    render(<Doc />);
    expect(screen.getByRole("button", { name: "Summary" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("heading", { name: "Network" })).toBeInTheDocument();
    press("YAML");
    expect(screen.getByRole("button", { name: "YAML" })).toHaveAttribute("aria-pressed", "true");
    expect(blockText()).toBe(toYaml(SPEC));
    press("JSON");
    expect(blockText()).toBe(JSON.stringify(SPEC, null, 2));
    expect(screen.queryByRole("heading", { name: "Network" })).toBeNull();
  });

  it("is controlled: a click asks for a view and changes nothing by itself", () => {
    const onViewChange = vi.fn();
    render(<PolicyDocument spec={SPEC} view="summary" onViewChange={onViewChange} />);
    press("JSON");
    expect(onViewChange).toHaveBeenCalledWith("json");
    expect(screen.getByRole("button", { name: "Summary" })).toHaveAttribute("aria-pressed", "true");
  });

  it("Summary → YAML → JSON → Summary leaves the authored source exactly as it was", () => {
    const onSource = vi.fn();
    function Draft() {
      const [text] = React.useState(YAML);
      onSource(text);
      return <Doc text={text} />;
    }
    render(<Draft />);
    for (const name of ["YAML", "JSON", "Summary"]) press(name);
    expect(new Set(onSource.mock.calls.map((c) => c[0]))).toEqual(new Set([YAML]));
    expect(screen.getByRole("heading", { name: "Network" })).toBeInTheDocument();
  });

  it("PolicyDocumentView opens on Summary and keeps its own view", () => {
    render(<PolicyDocumentView spec={SPEC} />);
    expect(screen.getByRole("heading", { name: "Barrier" })).toBeInTheDocument();
    press("YAML");
    expect(blockText()).toBe(toYaml(SPEC));
  });
});

describe("PolicyDocument — what each copy button copies", () => {
  it("with no authored text, Copy YAML and Copy JSON are the emitters' output", async () => {
    render(<Doc />);
    expect(await copied("Copy YAML")).toBe(toYaml(SPEC));
    press("YAML");
    expect(await copied("Copy YAML")).toBe(toYaml(SPEC));
    press("JSON");
    expect(screen.queryByRole("button", { name: "Copy YAML" })).toBeNull();
    expect(await copied("Copy JSON")).toBe(JSON.stringify(SPEC, null, 2));
  });

  it("authored YAML is copied as written from Summary and from its own view, comments included", async () => {
    render(<Doc text={YAML} />);
    expect(await copied("Copy YAML")).toBe(YAML);
    press("YAML");
    // Its own format's view shows the text as written, not a re-emitted one.
    expect(document.querySelector("pre code")?.textContent).toBe(YAML);
    expect(await copied("Copy YAML")).toBe(YAML);
    expect(await copied("Copy source")).toBe(YAML);
  });

  it("the other format is a normalised conversion, without the comments", async () => {
    render(<Doc text={YAML} />);
    press("JSON");
    const json = await copied("Copy JSON");
    expect(json).toBe(JSON.stringify(SPEC, null, 2));
    expect(json).not.toContain("#");
  });

  it("authored JSON is copied as written in the JSON view and converted for YAML", async () => {
    render(<Doc text={JSON_TEXT} format="json" />);
    expect(await copied("Copy YAML")).toBe(toYaml(SPEC));
    press("JSON");
    expect(document.querySelector("pre code")?.textContent).toBe(JSON_TEXT);
    expect(await copied("Copy JSON")).toBe(JSON_TEXT);
  });
});

describe("PolicyDocument — invalid, stale and hidden", () => {
  it("invalid authored text shows no synthesised policy; only the exact text can be copied", async () => {
    render(<Doc text={"a: ["} />);
    expect(screen.queryByRole("heading")).toBeNull();
    expect(screen.getByRole("button", { name: "Copy YAML" })).toBeDisabled();
    press("JSON");
    expect(document.querySelector("pre")).toBeNull();
    expect(screen.getByRole("button", { name: "Copy JSON" })).toBeDisabled();
    press("YAML");
    expect(document.querySelector("pre code")?.textContent).toBe("a: [");
    expect(screen.getByRole("button", { name: "Copy YAML" })).toBeDisabled();
    expect(await copied("Copy source")).toBe("a: [");
  });

  it("a stale policy stays readable with every generated copy held", () => {
    render(<Doc stale />);
    expect(screen.getByRole("heading", { name: "Network" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy YAML" })).toBeDisabled();
    press("YAML");
    expect(blockText()).toBe(toYaml(SPEC));
    for (const b of screen.getAllByRole("button", { name: /^Copy/ })) expect(b).toBeDisabled();
  });

  it("says values are hidden above the raw views only", () => {
    const note = "Values shown as <redacted> are hidden from you. Fill them in before using this as a policy.";
    render(<Doc redacted />);
    expect(screen.queryByText(note)).toBeNull();
    press("YAML");
    expect(screen.getByText(note)).toBeInTheDocument();
    press("JSON");
    expect(screen.getByText(note)).toBeInTheDocument();
  });

  it("renders the wrapper's notes and actions around the policy", () => {
    render(<Doc notes={<p>from the wrapper</p>} actions={<button type="button">Edit policy</button>} />);
    expect(screen.getByText("from the wrapper")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Edit policy" })).toBeInTheDocument();
  });
});
