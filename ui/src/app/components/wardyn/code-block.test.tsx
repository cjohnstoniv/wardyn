/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { toYaml, CodeBlock, JsonBlock, YamlBlock } from "./code-block";

describe("toYaml", () => {
  it("renders a policy spec as pretty, indented YAML (arrays of scalars + maps)", () => {
    const spec = {
      allowed_domains: ["api.anthropic.com", "*.githubusercontent.com"],
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC3",
      eligible_grants: [
        { kind: "api_key", scope: { host: "api.anthropic.com", format: "%s" }, requires_approval: false },
      ],
      workspace_repos: [{ repo: "https://github.com/sindresorhus/slugify" }],
      auto_stop_after_sec: 3600,
    };
    expect(toYaml(spec)).toBe(
      [
        "allowed_domains:",
        "  - api.anthropic.com",
        '  - "*.githubusercontent.com"', // leading * must be quoted
        "first_use_approval: deny_with_review",
        "min_confinement_class: CC3",
        "eligible_grants:",
        "  - kind: api_key", // list-of-maps: first key hoisted after "- "
        "    scope:",
        "      host: api.anthropic.com",
        '      format: "%s"', // % must be quoted
        "    requires_approval: false",
        "workspace_repos:",
        '  - repo: "https://github.com/sindresorhus/slugify"', // ":" must be quoted
        "auto_stop_after_sec: 3600",
      ].join("\n"),
    );
  });

  it("handles empties + null", () => {
    expect(toYaml({ a: [], b: {}, c: null, d: "" })).toBe(['a: []', "b: {}", "c: null", 'd: ""'].join("\n"));
  });

  // A raw line break ended the scalar, so the rest read back as another key; an
  // unquoted `true`, numeric or padded key read back as a different key. Either
  // way the emitted text no longer described the mapping it was made from.
  it("quotes strings holding a line break or control character, and ambiguous mapping keys", () => {
    expect(toYaml({ pattern: "echo one\necho two" })).toBe('pattern: "echo one\\necho two"');
    expect(toYaml({ a: ["x\r\ny", "bell\u0007", "nul\u0000"] })).toBe(
      ["a:", '  - "x\\r\\ny"', '  - "bell\\u0007"', '  - "nul\\u0000"'].join("\n"),
    );
    expect(toYaml({ true: "value" })).toBe('"true": value');
    expect(toYaml({ " padded ": 1, "a: b": 2, "": 3, "#x": 4, "1": 5, "line\nbreak": [{ "~": null }] })).toBe(
      ['"1": 5', '" padded ": 1', '"a: b": 2', '"": 3', '"#x": 4', '"line\\nbreak":', '  - "~": null'].join("\n"),
    );
  });

  it("keeps a tab inside a value and ordinary keys unquoted", () => {
    expect(toYaml({ pattern: "echo\tone", "x-api-key": "a b", "acme/one": 1 })).toBe(
      "pattern: echo\tone\nx-api-key: a b\nacme/one: 1",
    );
  });

  // YAML allows an implicit key at most 1024 characters before its colon.
  it("writes a key longer than 1024 characters in the explicit form", () => {
    const key = "k".repeat(1025);
    expect(toYaml({ [key]: 1, list: [{ [key]: { a: 1 } }] })).toBe(
      [`? ${key}`, ": 1", "list:", `  - ? ${key}`, "    :", "      a: 1"].join("\n"),
    );
    expect(toYaml({ ["k".repeat(1024)]: 1 })).toBe(`${"k".repeat(1024)}: 1`);
  });
});

// the copy button was opacity-0 + group-hover:opacity-100 only, so a
// keyboard user tabbing to it landed on a visually-invisible control (WCAG 2.4.7).
// focus-visible:opacity-100 reveals it on keyboard focus.
describe("copy button keyboard-focus reveal", () => {
  it("JsonBlock's copy button reveals on focus-visible, not just hover", () => {
    render(<JsonBlock value={{ a: 1 }} />);
    expect(screen.getByRole("button", { name: /copy/i }).className).toMatch(/focus-visible:opacity-100/);
  });

  it("YamlBlock's copy button reveals on focus-visible, not just hover", () => {
    render(<YamlBlock value={{ a: 1 }} />);
    expect(screen.getByRole("button", { name: /copy/i }).className).toMatch(/focus-visible:opacity-100/);
  });
});

// JsonBlock's copy button routes through useCopyToClipboard's `copy()` — these
// pin the two honesty properties end to end, not just at the hook.
describe("JsonBlock — copy feedback is honest about clipboard availability", () => {
  it("never announces 'Copied' when navigator.clipboard is unavailable (LAN HTTP / insecure context)", async () => {
    Object.assign(navigator, { clipboard: undefined });
    render(<JsonBlock value={{ a: 1 }} />);

    fireEvent.click(screen.getByRole("button", { name: /copy/i }));

    // The rejected copyAsync() settles on a microtask; give it a tick — it
    // must never flip `copied`, so "Copied" must never appear.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.queryByText("Copied")).not.toBeInTheDocument();
  });

  it("announces 'Copied' in a polite aria-live region once the write actually resolves", async () => {
    Object.assign(navigator, { clipboard: { writeText: vi.fn().mockResolvedValue(undefined) } });
    render(<JsonBlock value={{ a: 1 }} />);

    fireEvent.click(screen.getByRole("button", { name: /copy/i }));

    const live = await screen.findByText("Copied");
    expect(live).toHaveAttribute("aria-live", "polite");
  });
});

describe("CodeBlock copyLabel", () => {
  it("names the Copy button, and defaults to Copy without it", () => {
    render(
      <>
        <CodeBlock text="a" copyLabel="Copy ssh config" />
        <CodeBlock text="b" />
      </>,
    );
    expect(screen.getByRole("button", { name: "Copy ssh config", hidden: true })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy", hidden: true })).toBeInTheDocument();
  });
});
