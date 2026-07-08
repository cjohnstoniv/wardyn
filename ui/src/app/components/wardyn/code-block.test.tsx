/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { toYaml } from "./code-block";

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
