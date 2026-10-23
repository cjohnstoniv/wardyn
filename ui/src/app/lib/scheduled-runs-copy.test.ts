/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Doc/code byte parity for the scheduled-run Runs list and run page strings.

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { NR_RUN } from "../components/wardyn/copy/new-run/run";
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
    const sources = readFileSync(DOC, "utf8").split("\n").filter((l) => /^\| `[A-Z_]+\b/.test(l) && !l.includes("`NR_")).map((l) => l.split("|")[2].trim());
    expect(sources.every((s) => s === "mock" || /^deviation \(FEAT-2 board F2SCHED-[23]\)$/.test(s))).toBe(true);
  });

  // Rows held by new-run-canon.md, recorded here for the ruling behind each: key (in its namespace) -> source.
  const HELD = {
    "NR_REVIEW.TIMING_CHOOSE": "deviation (derived from board F2SCHED-1's logic, not its copy box; approved 2026-10-20)",
    "NR_RUN.START_HELP(n)": "deviation (computed from the server's lead wait; overseer ruling 2026-10-21)",
    "NR_RUN.START_FAR(n)": "deviation (computed from the server's lead wait; overseer ruling 2026-10-21)",
  } as const;

  it.each(Object.entries(HELD))("records %s with its ruling, the code's string, and the same source in the New Run canon", (key, source) => {
    const row = readFileSync(DOC, "utf8").split("\n").find((l) => l.startsWith(`| \`${key}\``)) ?? "";
    const [, , rowSource, string] = row.split("|").map((c) => c.trim());
    expect(rowSource).toBe(source);
    const [ns, name] = key.split(".");
    expect(string).toBe(renderFromNamespaces(name, [ns === "NR_RUN" ? NR_RUN : NR_REVIEW]));
    const newRun = readFileSync(resolve(process.cwd(), "../docs/design/new-run-canon.md"), "utf8");
    expect(newRun).toContain(`| \`${name}\` | ${source} |`);
  });
});
