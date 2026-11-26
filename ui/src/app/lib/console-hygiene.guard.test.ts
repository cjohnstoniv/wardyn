/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join, relative } from "node:path";
import { NR_RUN } from "../components/wardyn/copy/new-run/run";
import { NR_TOOLS } from "../components/wardyn/copy/new-run/tools";

const src = join(__dirname, "..", "..");
const app = join(src, "app");
const files = (dir: string): string[] =>
  readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = join(dir, e.name);
    if (e.isDirectory()) return files(p);
    return /\.tsx?$/.test(p) ? [p] : [];
  });
const isTest = (p: string) => /\.test\.tsx?$/.test(p);
const appSources = () => files(app).filter((p) => !isTest(p));

// Text sizes come from the type scale (theme.css: text-meta 11px, text-body 13px, then text-xs and text-sm). These
// files may still carry an arbitrary text-[Npx]: sizes below the scale, or the 15px title, pending a design call.
const ARBITRARY_SIZE_ALLOWED: Record<string, string> = {
  "components/screens/new-run-flow/launch/launched-screen.tsx": "22px glyph",
  "components/screens/new-run-flow/steps/files/add-dialog.tsx": "15px source name",
  "components/screens/new-run-flow/steps/files/attach-card.tsx": "10px pill",
  "components/screens/new-run-flow/steps/files/computer-folder.tsx": "15px computer name",
  "components/screens/new-run-flow/steps/files/kind-visuals.tsx": "10px pill",
  "components/screens/new-run-flow/steps/run/pool-summary.tsx": "9px and 10px metric labels",
  "components/screens/providers/entra-editor.tsx": "10px pill",
  "components/screens/run-detail/desktop-replay.tsx": "15px pause title",
  "components/screens/runs.tsx": "10px chevron",
  "components/wardyn/step-section.tsx": "15px step title",
  "lib/test-dom.ts": "reads the step title's 15px class",
};

// Keys kept with no consumer yet: strings written ahead of the code that will show them (the pool select's group
// words; canon rows marked pending), and the canon's quote of the Wall mechanism line, which cc-meta.test.ts checks.
const KEPT_UNRENDERED: Record<string, string[]> = {
  NR_RUN: ["POOLS_ORG", "POOLS_USER", "PLACEMENT_DENIED", "BARRIER_NO_TERMINAL", "WALL_HOW"],
  NR_TOOLS: ["IMAGE_NO", "IMAGE_UNKNOWN", "PROFILE_REQUIRES", "NET_POLICY_PROFILE", "NET_DECIDER", "DELIVERY_AZURE", "DELIVERY_AWS"],
};

describe("console source hygiene", () => {
  it("no text-[11px] or text-[13px]: they are text-meta and text-body", () => {
    const offenders = appSources().flatMap((p) =>
      readFileSync(p, "utf8")
        .split("\n")
        .flatMap((line, i) => (/text-\[1[13]px\]/.test(line) ? [`${relative(app, p)}:${i + 1}`] : [])),
    );
    expect(offenders).toEqual([]);
  });

  it("an arbitrary text-[Npx] size only in the allowlisted files", () => {
    const offenders = appSources()
      .filter((p) => !(relative(app, p) in ARBITRARY_SIZE_ALLOWED))
      .filter((p) => /text-\[\d+(\.\d+)?px\]/.test(readFileSync(p, "utf8")))
      .map((p) => relative(app, p));
    expect(offenders).toEqual([]);
  });

  it("no run fixture is cast: build it with makeRun or a typed literal", () => {
    const offenders = files(src)
      .filter((p) => relative(src, p) !== "app/lib/console-hygiene.guard.test.ts")
      .flatMap((p) =>
        readFileSync(p, "utf8")
          .split("\n")
          .flatMap((line, i) => (/\bas AgentRun\b/.test(line) ? [`${relative(src, p)}:${i + 1}`] : [])),
      );
    expect(offenders).toEqual([]);
  });

  it.each([
    ["NR_RUN", NR_RUN, "components/wardyn/copy/new-run/run.ts"],
    ["NR_TOOLS", NR_TOOLS, "components/wardyn/copy/new-run/tools.ts"],
  ] as const)("every %s key has a consumer outside its module and the tests", (name, ns, module) => {
    const used = appSources()
      .filter((p) => relative(app, p) !== module)
      .map((p) => readFileSync(p, "utf8"))
      .join("\n");
    const dead = Object.keys(ns).filter((k) => !KEPT_UNRENDERED[name].includes(k) && !new RegExp(`\\b${k}\\b`).test(used));
    expect(dead).toEqual([]);
  });
});
