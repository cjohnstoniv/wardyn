/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

// docs/design/CONSOLE-RULES.md opens with "Every rule names the token or
// component that implements it, so review is a lookup, not an argument" —
// but a code citation that drifts silently turns the lookup into a lie. This
// suite is the forward ratchet the doc's own opening line promises.
//
// It used to check LINE numbers: every `file.ext:N[–M]` anchor had to land
// inside the cited file, and a hand-listed handful had to contain the named
// symbol's declaration. Two things were wrong with that. The anchors rotted
// constantly — F040 found ten stale primitives.tsx citations, R4-E2E-B2 found
// the same failure in states.tsx, F041 found a citation past runs.tsx's EOF —
// because ANY insertion above a cited line moves it. And the resolver needed a
// hand-maintained basename→directory map, so a citation into a file nobody had
// added to the map was silently out of scope: 5 of 20 cited basenames were
// covered when U-02 looked.
//
// Both are gone. Citations name a SYMBOL (`file.tsx#Symbol`, a custom property
// or selector for CSS), the cited file is found by globbing the two source
// roots, and every citation in the doc is in scope by construction.
const DOC_PATH = resolve(process.cwd(), "../docs/design/CONSOLE-RULES.md");
const doc = readFileSync(DOC_PATH, "utf8");

// The two roots every console citation lands in. Globbed, not enumerated — a
// component moved between wardyn/, screens/ and ui/ keeps resolving, and a new
// directory under them is covered the day it is created.
const ROOTS = [resolve(process.cwd(), "src/app/components"), resolve(process.cwd(), "src/styles")];

function walk(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = resolve(dir, e.name);
    return e.isDirectory() ? walk(p) : /\.(tsx?|css)$/.test(e.name) ? [p] : [];
  });
}
const SOURCES = ROOTS.flatMap(walk);

/** cited path (basename, or enough trailing segments to be unique) -> its lines. */
const fileLines = new Map<string, string[]>();
function linesOf(cited: string): string[] {
  let ls = fileLines.get(cited);
  if (!ls) {
    const hits = SOURCES.filter((p) => p.endsWith(`/${cited}`));
    if (hits.length !== 1) {
      throw new Error(
        `console-rules-citations.test.ts: "${cited}" matches ${hits.length} files under ${ROOTS.join(", ")}` +
          (hits.length > 1 ? ` — cite enough path segments to be unique (${hits.join(", ")})` : ""),
      );
    }
    ls = readFileSync(hits[0], "utf8").split("\n");
    fileLines.set(cited, ls);
  }
  return ls;
}

// `<path>#<Symbol>` — one or more comma-separated symbols, the way a rule that
// rests on two adjacent base-layer declarations cites both.
const CITATION_RE = /`([\w/-]+\.(?:tsx?|css))#([\w.$-]+(?:,[\w.$-]+)*)`/g;
const citations: { file: string; symbol: string }[] = [];
for (const m of doc.matchAll(CITATION_RE)) {
  for (const symbol of m[2].split(",")) citations.push({ file: m[1], symbol });
}

/**
 * Whether `file` declares `symbol` at the top level: a named function, const,
 * let, var, class, type or interface in TS/TSX; a custom property or a selector
 * at the head of a rule in CSS. Re-derived from the source on every run — there
 * is no line number to go stale, which is the whole point of the form.
 */
function declares(file: string, symbol: string): boolean {
  const lines = linesOf(file);
  if (file.endsWith(".css")) {
    // `--radius: …`, `label { … }`, `.label-eyebrow {`. The boundary after the
    // symbol is load-bearing: without it `--radius` would match `--radius-sm`.
    const re = new RegExp(`^${escape(symbol)}(?=\\s*[:{,]|\\s)`);
    return lines.some((l) => re.test(l.trim()));
  }
  const re = new RegExp(`^(export\\s+)?(default\\s+)?(async\\s+)?(function|const|let|var|class|type|interface|enum)\\s+${escape(symbol)}\\b`);
  return lines.some((l) => re.test(l));
}
function escape(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\-]/g, "\\$&");
}

describe("CONSOLE-RULES.md citations name symbols that still exist", () => {
  it("found at least the citations this doc is known to carry (guards against the regex silently matching nothing)", () => {
    // 52 resolve today (51 backtick spans; one names two adjacent base-layer
    // selectors). The floor sits below that so deliberately deleting a rule's
    // citation is not a test failure, and far above zero so a regex that stops
    // matching — the failure this case exists for — still is.
    expect(citations.length).toBeGreaterThanOrEqual(40);
  });

  it.each(citations.map((c) => [`${c.file}#${c.symbol}`, c] as const))(
    "%s resolves to exactly one source file, which declares it",
    (_label, c) => {
      expect(declares(c.file, c.symbol)).toBe(true);
    },
  );

  // The rule the tree already enforces on Go comments, threatmodel/*.md and
  // docs/AUDIT-ACTIONS.md (TestCommentsCiteSymbolsNotLineNumbers and its
  // neighbours): a claim about code is pinned to a symbol, never to a line.
  // Without this the old form creeps back one rule at a time — and it is also
  // what retires F041's `runs.tsx:811`, a citation 139 lines past that file's
  // own EOF, as a class rather than as one hardcoded string.
  it("pins no claim to a line number", () => {
    const lineAnchored = [...doc.matchAll(/[\w/-]+\.(?:tsx?|css|go|md):\d+/g)].map((m) => m[0]);
    expect(lineAnchored).toEqual([]);
  });
});

// F041: the §2 "Known violations" register named two runs.tsx text-primary
// sites that were already fixed. Pin the current, corrected claim so a future
// accidental re-add of text-primary fails here instead of silently rotting.
describe("CONSOLE-RULES.md §2's known-violations register matches runs.tsx", () => {
  const runsPath = resolve(process.cwd(), "src/app/components/screens/runs.tsx");
  const runsSrc = readFileSync(runsPath, "utf8");

  it("runs.tsx carries no text-primary (the two former violations are fixed)", () => {
    expect(runsSrc).not.toMatch(/text-primary/);
  });

  it("the register no longer cites a runs.tsx violation", () => {
    expect(doc).not.toMatch(/`runs\.tsx#[\w.$-]+`[^\n]*text-primary/);
  });
});
