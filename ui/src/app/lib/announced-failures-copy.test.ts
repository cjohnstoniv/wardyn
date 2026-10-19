/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { RECORDING_DISABLED_TITLE } from "../components/wardyn/copy/recording-disabled";
import { OPERATOR_ONLY_REASON } from "../components/wardyn/copy";
import { RECORDINGS } from "../components/screens/recording-copy";
import { AUDIT } from "../components/screens/audit-copy";
import { parseFrozenTables } from "./copy-doc-parity";

// The mock round's whole value is that it stays CHECKABLE (the sign-in/
// ado-entra precedent, copy-doc-parity.ts's parseFrozenTables(), T-66): this
// suite PARSES docs/design/announced-failures-canon.md's "## Frozen strings"
// table back out of the doc and compares every key.
//
// Owner ruling 2026-09-25 (#726): this doc has no single owning module — its
// rows are bound here across real modules (use-intent-checks.ts, wardyn/
// copy.ts, recording-copy.ts, recording-disabled.ts) plus audit-copy.ts, created by this same
// ruling to carry AUDIT.MEMBER_FEED_TITLE, which was an inline JSX literal in
// audit.tsx's AuditScreen with no exported constant.
//
// Two doc-specific quirks, normalized here rather than in the shared parser:
//   - The Key cell carries a trailing "(file.ts)" annotation
//     ("RECORDING_DISABLED_TITLE (wardyn/copy/recording-disabled.ts)") rather than a
//     separate column — stripped by a trailing " (...)" cut.
//   - Every String cell is wrapped in literal double quotes ("No reason
//     was given.") rather than plain text — stripped below. Neither cut can
//     touch a real string, since none of these frozen strings contain
//     a parenthesis or a leading/trailing quote of their own.
const DOC = resolve(process.cwd(), "../docs/design/announced-failures-canon.md");

const stripKeyNote = (s: string): string => s.replace(/ \([^)]*\)$/, "");
const stripQuotes = (s: string): string => s.replace(/^"(.*)"$/, "$1");

const rawDoc = parseFrozenTables(DOC, /^## Frozen strings/);
const doc = new Map([...rawDoc].map(([key, value]) => [stripKeyNote(key), stripQuotes(value)]));

// #497's check-error fallback is an inline literal inside checkFailureText in use-intent-checks.ts
// (the preflight blocker and the Tools step's failed-preview alert both call it), so it is read out
// of the source text, anchored on that function.
const CHECK_FALLBACK =
  /checkFailureText = [^\n]*getErrorMessage\(error\) \|\| "([^"]*)"/.exec(
    readFileSync(resolve(process.cwd(), "src/app/components/screens/new-run-flow/draft/use-intent-checks.ts"), "utf8"),
  )?.[1] ?? "";

const rendered: Record<string, string> = {
  OPERATOR_ONLY_REASON: OPERATOR_ONLY_REASON,
  "RECORDINGS.EMPTY_TITLE": RECORDINGS.EMPTY_TITLE,
  "RECORDINGS.EMPTY_BODY": RECORDINGS.EMPTY_BODY,
  RECORDING_DISABLED_TITLE: RECORDING_DISABLED_TITLE,
  "AUDIT.MEMBER_FEED_TITLE": AUDIT.MEMBER_FEED_TITLE,
  "Check error fallback": CHECK_FALLBACK,
};

describe("announced-failures-copy — announced-failures-canon.md, #490", () => {
  it("finds all 6 frozen keys in the doc", () => {
    expect(doc.size).toBe(6);
  });

  it.each(Object.keys(rendered))("%s is byte-exact", (key) => {
    expect(rendered[key]).toBe(doc.get(key));
  });

  it("covers every doc key, and freezes no key the doc doesn't", () => {
    expect(Object.keys(rendered).sort()).toEqual([...doc.keys()].sort());
  });
});
