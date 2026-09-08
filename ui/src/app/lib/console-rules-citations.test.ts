/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

// docs/design/CONSOLE-RULES.md opens with "Every rule names the token or
// component that implements it, so review is a lookup, not an argument" —
// but a code citation that drifts silently turns the lookup into a lie. This
// suite is the forward ratchet the doc's own opening line promises: every
// `file.ext:N` or `file.ext:N–M` anchor it makes into a known source file
// must still land inside that file (LINE-BEYOND-EOF is exactly how F041's
// dead `runs.tsx:811` citation was found — runs.tsx is 672 lines), and the
// handful of citations phrased as `Symbol` (`file.tsx:N[–M]`) — a symbol
// immediately followed by its own citation, no qualifier in between — must
// have that symbol's declaration inside the cited range (this is exactly the
// class of drift F040 found: ten primitives.tsx citations landing on
// unrelated code after the file grew around them).

// process.cwd() is the vitest root — ui/ — for every entry point that runs
// this suite (matches governance-copy.test.ts's DOC resolution).
const DOC_PATH = resolve(process.cwd(), "../docs/design/CONSOLE-RULES.md");
const doc = readFileSync(DOC_PATH, "utf8");

const WARDYN_DIR = resolve(process.cwd(), "src/app/components/wardyn");
const SCREENS_DIR = resolve(process.cwd(), "src/app/components/screens");
const STYLES_DIR = resolve(process.cwd(), "src/styles");

// F040/R4-E2E-B2: every basename this guard knows how to resolve, and the
// directory it lives in. Started as primitives.tsx/form-primitives.tsx only
// (the wardyn/ pattern layer, where F040 found ten stale citations); widened
// to states.tsx, app-shell.tsx and theme.css once R4-E2E-B2 found the same
// stale-anchor failure mode sitting outside that filter, uncaught.
const FILE_DIRS: Record<string, string> = {
  "primitives.tsx": WARDYN_DIR,
  "form-primitives.tsx": WARDYN_DIR,
  "states.tsx": WARDYN_DIR,
  "app-shell.tsx": SCREENS_DIR,
  "theme.css": STYLES_DIR,
};

/** file basename (as cited in the doc, e.g. "primitives.tsx") -> its lines. */
const fileLines = new Map<string, string[]>();
function linesOf(basename: string): string[] {
  let ls = fileLines.get(basename);
  if (!ls) {
    const dir = FILE_DIRS[basename];
    if (!dir) throw new Error(`console-rules-citations.test.ts: no FILE_DIRS entry for "${basename}"`);
    ls = readFileSync(resolve(dir, basename), "utf8").split("\n");
    fileLines.set(basename, ls);
  }
  return ls;
}

// Every backtick-fenced `<basename>.<tsx|css>:<N>` or `<basename>.<tsx|css>:<N>–<M>`
// citation into a file this suite knows how to resolve (FILE_DIRS above).
// Anything else cited in the doc is out of scope for this guard.
//
// R4-E2E-2-B1: the basename class is `[\w-]+`, NOT `\w+` — a hyphen is a legal
// basename character and two of FILE_DIRS' own five entries carry one
// (`form-primitives.tsx`, `app-shell.tsx`), so `\w+` silently excluded every
// citation into them from the bounds check while FILE_DIRS advertised them as
// covered. That blind spot is why `app-shell.tsx:200–206, 241` (navLinkClass)
// could rot 86 lines out of date under a guard that claimed to watch the file.
const CITATION_RE = /`([\w-]+\.(?:tsx|css)):(\d+)(?:[–-](\d+))?`/g;
type Citation = { file: string; start: number; end: number; index: number };
const citations: Citation[] = [];
for (const m of doc.matchAll(CITATION_RE)) {
  if (!(m[1] in FILE_DIRS)) continue;
  const start = Number(m[2]);
  const end = m[3] ? Number(m[3]) : start;
  citations.push({ file: m[1], start, end, index: m.index! });
}

describe("CONSOLE-RULES.md citations into known source files stay in bounds", () => {
  it("found at least the citations this doc is known to carry (guards against the regex silently matching nothing)", () => {
    // 26 in-scope citations resolve today (20 before R4-E2E-2-B1 widened
    // CITATION_RE to hyphenated basenames and split the multi-anchor nav-item
    // citation). The floor sits below that so deliberately deleting a rule's
    // citation is not a test failure, and far above zero so a regex that stops
    // matching — the failure this case exists for — still is.
    expect(citations.length).toBeGreaterThanOrEqual(20);
  });

  it.each(citations.map((c) => [`${c.file}:${c.start}${c.end !== c.start ? `–${c.end}` : ""}`, c] as const))(
    "%s is not LINE-BEYOND-EOF and start <= end",
    (_label, c) => {
      const total = linesOf(c.file).length;
      expect(c.start).toBeGreaterThanOrEqual(1);
      expect(c.start).toBeLessThanOrEqual(c.end);
      expect(c.end).toBeLessThanOrEqual(total);
    },
  );
});

// The tighter check: a handful of CONSOLE-RULES.md citations name the exact
// symbol whose declaration the range should contain, with nothing but the
// symbol's own backticks between the name and the citation (or one glyph
// word, e.g. "monogram") — for those, re-derive the declaration line from the
// cited source file itself (never hardcode it) and assert the doc's citation
// still covers it.
function declarationLine(file: string, symbol: string): number {
  const ls = linesOf(file);
  const re = new RegExp(`^(export )?(function|const) ${symbol}\\b`);
  const idx = ls.findIndex((l) => re.test(l));
  if (idx === -1) throw new Error(`console-rules-citations.test.ts: "${symbol}" not found in ${file} — update the fixture list below`);
  return idx + 1; // 1-indexed to match the doc's citations
}

describe("CONSOLE-RULES.md citations name the right symbol", () => {
  it.each([
    ["primitives.tsx", "ConfinementChip"],
    ["primitives.tsx", "AgentBadge"],
    ["primitives.tsx", "RunStateBadge"],
    ["primitives.tsx", "OperatorOnlyHint"],
    ["primitives.tsx", "metaFor"],
    // R4-E2E-B2: F040's failure mode (a citation landing on unrelated code
    // once the file grew) recurred in states.tsx — ErrorState's citation had
    // drifted onto EmptyState's closing tag, TableSkeleton's onto
    // TruncatedNote's body.
    ["states.tsx", "ErrorState"],
    ["states.tsx", "TableSkeleton"],
    // R4-E2E-2-B1: the same drift survived on three more sites of this exact
    // class in this exact file. `TruncatedNote` was cited at states.tsx:65 —
    // a line INSIDE ErrorState's body, one line after the citation B2 had
    // just corrected — and `navLinkClass` at app-shell.tsx:200–206, 241,
    // which the multi-anchor form (a comma between the two ranges, so no
    // closing backtick after the digits) hid from this guard entirely. The
    // doc now fences the two nav anchors separately, which is what makes
    // navLinkClass checkable here at all.
    ["states.tsx", "TruncatedNote"],
    ["app-shell.tsx", "navLinkClass"],
  ] as const)("%s's %s declaration line is inside every citation the doc makes for it", (file, symbol) => {
    const declLine = declarationLine(file, symbol);
    // Every citation of the exact form `Symbol` ... (`<file>:N[–M]`) within
    // ~80 chars (covers "monogram", "tones", a hard-wrapped line break before
    // the citation, etc. in between).
    const escapedFile = file.replace(/\./g, "\\.");
    const re = new RegExp("`" + symbol + "`[\\s\\S]{0,80}?`" + escapedFile + ":(\\d+)(?:[–-](\\d+))?`", "g");
    const matches = [...doc.matchAll(re)];
    expect(matches.length).toBeGreaterThan(0);
    for (const m of matches) {
      const start = Number(m[1]);
      const end = m[2] ? Number(m[2]) : start;
      expect(declLine).toBeGreaterThanOrEqual(start);
      expect(declLine).toBeLessThanOrEqual(end);
    }
  });
});

// F041: the §2 "Known violations" register named two runs.tsx text-primary
// sites that were already fixed (one of them past runs.tsx's own EOF). Pin
// the current, corrected claim so a future accidental re-add of text-primary
// (or a re-stale line number) fails here instead of silently rotting again.
describe("CONSOLE-RULES.md §2's known-violations register matches runs.tsx", () => {
  const runsPath = resolve(process.cwd(), "src/app/components/screens/runs.tsx");
  const runsSrc = readFileSync(runsPath, "utf8");

  it("runs.tsx carries no text-primary (the two former violations are fixed)", () => {
    expect(runsSrc).not.toMatch(/text-primary/);
  });

  it("the register no longer cites a runs.tsx violation", () => {
    expect(doc).not.toMatch(/`runs\.tsx:\d+`[^\n]*text-primary/);
    expect(doc).not.toContain("`runs.tsx:811`");
  });
});
