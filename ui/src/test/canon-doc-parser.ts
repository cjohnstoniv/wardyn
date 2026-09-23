/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";

// Shared by every copy-module canon suite (the ado-entra-copy.test.ts
// pattern, T-66): each suite does not hand-retype a sample of its design
// doc's frozen-string table — it PARSES the doc back out and compares every
// key against its copy module. A swapped hyphen, a dropped ellipsis, a
// reworded clause, a new doc row or a deleted one all fail there rather than
// shipping. Four suites (governance-copy, ado-entra-copy, user-drives-copy,
// workspace-providers-copy) carried byte-identical copies of this function,
// differing only in the doc path and which `### N.n` headings hold the
// frozen tables — this is that one function, parameterized on both.
//
// Backticks are stripped from every doc cell. Each design doc's §7 header
// note makes mono a DISPLAY concern applied by the consuming component; the
// frozen string itself is plain text, whether or not a doc's rows happen to
// use backticks as real content (user-drives-prompt.md's mount targets and
// env vars do; that is why unmono() is not a no-op there).
const unmono = (s: string) => s.replace(/`/g, "");

/**
 * key -> frozen string, for every row of every markdown table under a
 * heading `sectionHeading` matches, in the doc at `docPath`.
 *
 * `docPath` is resolved by the caller (`resolve(process.cwd(), ...)` — every
 * entry point that runs a vitest suite has `ui/` as `process.cwd()`).
 */
export function parseFrozenTables(docPath: string, sectionHeading: RegExp): Map<string, string> {
  const rows = new Map<string, string>();
  let inSection = false;
  for (const line of readFileSync(docPath, "utf8").split("\n")) {
    if (line.startsWith("#")) {
      inSection = sectionHeading.test(line);
      continue;
    }
    if (!inSection || !line.startsWith("|")) continue;
    const cells = line.split("|").slice(1, -1).map((c) => c.trim());
    if (cells[0] === "Key") continue; // header
    if (/^:?-+:?$/.test(cells[0])) continue; // separator
    rows.set(unmono(cells[0]), unmono(cells[cells.length - 1]));
  }
  return rows;
}
