/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Doc/code byte parity for the scheduled-run Runs list and run page strings.

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { NR_REVIEW } from "../components/wardyn/copy/new-run/review";
import { SCHEDULED_RUN } from "../components/wardyn/copy/scheduled-runs";
import { parseFrozenTables, renderFromNamespaces, splitKey } from "./copy-doc-parity";

const DOC = resolve(process.cwd(), "../docs/design/scheduled-runs-canon.md");
const doc = parseFrozenTables(DOC, /^## 1\. /);

describe("scheduled runs canon", () => {
  it("covers every key, and freezes no key the doc doesn't", () => {
    expect([...doc.keys()].map((k) => splitKey(k)[0]).sort()).toEqual(Object.keys(SCHEDULED_RUN).sort());
  });

  it.each([...doc.keys()])("%s is byte-exact", (key) => {
    expect(renderFromNamespaces(key, [SCHEDULED_RUN])).toBe(doc.get(key));
  });

  it("every row names its board", () => {
    const sources = readFileSync(DOC, "utf8").split("\n").filter((l) => /^\| `[A-Z_]+\b/.test(l) && !l.includes("NR_REVIEW.")).map((l) => l.split("|")[2].trim());
    expect(sources.every((s) => s === "mock" || /^deviation \(FEAT-2 board F2SCHED-[23]\)$/.test(s))).toBe(true);
  });

  it("records the approved Review row sentence, and the same source in the New Run canon", () => {
    const row = readFileSync(DOC, "utf8").split("\n").find((l) => l.startsWith("| `NR_REVIEW.TIMING_CHOOSE`")) ?? "";
    const [, , source, string] = row.split("|").map((c) => c.trim());
    expect(source).toBe("deviation (derived from board F2SCHED-1's logic, not its copy box; approved 2026-10-20)");
    expect(string).toBe(NR_REVIEW.TIMING_CHOOSE);
    const newRun = readFileSync(resolve(process.cwd(), "../docs/design/new-run-canon.md"), "utf8");
    expect(newRun).toContain(`| \`TIMING_CHOOSE\` | ${source} |`);
  });
});
