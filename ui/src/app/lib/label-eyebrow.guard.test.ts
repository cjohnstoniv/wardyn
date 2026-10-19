/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join, relative } from "node:path";

// One look for a section micro-label (#3091): `.label-eyebrow`, or `.label-eyebrow-primary` beside it
// for the step eyebrow. A new hand-rolled "uppercase + tracking-*" label fails here; a chip, a
// mock-pinned New Run label or a pseudo-element label belongs in ALLOWED with its reason.
const ALLOWED: Record<string, string> = {
  "components/wardyn/primitives.tsx": "uppercase inside a Chip (run state badge)",
  "components/wardyn/live-approvals.tsx": "the warning-coloured 'waiting' tag",
  "components/screens/new-run-flow/review/barrier-chip.tsx": "the barrier chip's caps mode",
  "components/screens/setup/environment-step.tsx": "a Chip's own uppercase",
  "components/screens/top-bar.tsx": "a Chip's own uppercase",
  "components/screens/providers/entra-editor.tsx": "a pill, not a section label",
  "components/screens/profile-review.tsx": "the warning-coloured label",
  "components/screens/runners/pools/pools-tab.tsx": "a ::before label inside a Tailwind variant",
  "components/screens/new-run-flow/steps/run/pool-summary.tsx": "mock-pinned New Run metric label",
  "components/screens/new-run-flow/steps/run/question-block.tsx": "mock-pinned 'Required' annotation",
  "components/screens/new-run-flow/steps/files/kind-visuals.tsx": "mock-pinned Files group heading",
  "components/screens/new-run-flow/steps/files/main-drive-section.tsx": "mock-pinned Files aside",
};

const root = join(__dirname, "..");
const sources = (dir: string): string[] =>
  readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = join(dir, e.name);
    if (e.isDirectory()) return sources(p);
    return p.endsWith(".tsx") && !p.endsWith(".test.tsx") ? [p] : [];
  });

describe("section micro-labels use label-eyebrow", () => {
  it("no hand-rolled uppercase + tracking-* outside the allowlist", () => {
    const offenders = sources(root)
      .filter((p) => !(relative(root, p) in ALLOWED))
      .filter((p) => /uppercase[^"`\n]*tracking-|tracking-[^"`\n]*uppercase/.test(readFileSync(p, "utf8")))
      .map((p) => relative(root, p));
    expect(offenders).toEqual([]);
  });

  it("only one primary-colour eyebrow variant exists", () => {
    const css = readFileSync(join(root, "..", "styles", "theme.css"), "utf8");
    expect(css.match(/^\s*\.label-eyebrow[\w-]*\s*\{/gm)?.map((m) => m.trim())).toEqual([".label-eyebrow {", ".label-eyebrow-primary {"]);
  });
});
