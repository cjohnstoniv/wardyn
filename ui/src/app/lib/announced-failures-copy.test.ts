/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { RAIL, RECORDING_DISABLED_TITLE } from "../components/wardyn/copy/new-run-rail";
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
// 8 rows are bound here across 4 real modules (new-run-rail.ts, wardyn/
// copy.ts, recording-copy.ts) plus a 5th, audit-copy.ts, created by this same
// ruling to carry AUDIT.MEMBER_FEED_TITLE, which was an inline JSX literal at
// audit.tsx:529 with no exported constant.
//
// Two doc-specific quirks, normalized here rather than in the shared parser:
//   - The Key cell carries a trailing "(file.ts)" annotation
//     ("RAIL.LAUNCH_ERROR_LABEL (wardyn/copy/new-run-rail.ts)") rather than a
//     separate column — stripped by a trailing " (...)" cut.
//   - Every String cell is wrapped in literal double quotes ("Launch
//     failed") rather than plain text — stripped below. Neither cut can
//     touch a real string, since none of these eight frozen strings contain
//     a parenthesis or a leading/trailing quote of their own.
const DOC = resolve(process.cwd(), "../docs/design/announced-failures-canon.md");

const stripKeyNote = (s: string): string => s.replace(/ \([^)]*\)$/, "");
const stripQuotes = (s: string): string => s.replace(/^"(.*)"$/, "$1");

const rawDoc = parseFrozenTables(DOC, /^## Frozen strings/);
const doc = new Map([...rawDoc].map(([key, value]) => [stripKeyNote(key), stripQuotes(value)]));

const rendered: Record<string, string> = {
  "RAIL.LAUNCH_ERROR_LABEL": RAIL.LAUNCH_ERROR_LABEL,
  "RAIL.PREFLIGHT_ERROR_LABEL": RAIL.PREFLIGHT_ERROR_LABEL,
  OPERATOR_ONLY_REASON: OPERATOR_ONLY_REASON,
  "RECORDINGS.SEARCH_DISABLED_HINT": RECORDINGS.SEARCH_DISABLED_HINT,
  "RECORDINGS.EMPTY_TITLE": RECORDINGS.EMPTY_TITLE,
  "RECORDINGS.EMPTY_BODY": RECORDINGS.EMPTY_BODY,
  RECORDING_DISABLED_TITLE: RECORDING_DISABLED_TITLE,
  "AUDIT.MEMBER_FEED_TITLE": AUDIT.MEMBER_FEED_TITLE,
};

describe("announced-failures-copy — announced-failures-canon.md, #490", () => {
  it("finds all 8 frozen keys in the doc", () => {
    expect(doc.size).toBe(8);
  });

  it.each(Object.keys(rendered))("%s is byte-exact", (key) => {
    expect(rendered[key]).toBe(doc.get(key));
  });

  it("covers every doc key, and freezes no key the doc doesn't", () => {
    expect(Object.keys(rendered).sort()).toEqual([...doc.keys()].sort());
  });
});
