/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { toYaml, JsonBlock, YamlBlock } from "./code-block";

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
