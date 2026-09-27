/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { NAV, UNSAVED } from "./unsaved-copy";
import { PROVIDERS, PROVIDERS_DRAFT } from "./workspace-providers-copy";
import { parseFrozenTables } from "./copy-doc-parity";

// The mock round's whole value is that it stays CHECKABLE (the sign-in/
// ado-entra precedent, copy-doc-parity.ts's parseFrozenTables(), T-66): this
// suite does not hand-retype a sample of the canon — it PARSES
// docs/design/unsaved-guard-canon.md's "## 1. Frozen strings" table back out
// of the doc and compares every key. A swapped hyphen, a dropped ellipsis, a
// reworded clause, a new doc row or a deleted one all fail here rather than
// shipping.
//
// The doc's own header note already says where each key actually lives:
// UNSAVED.* and NAV.* are re-exported from unsaved-copy.ts (itself sourced
// from wardyn/copy/shell.ts's UNSAVED_GUARD, which names UNSAVED.DISCARD
// "LEAVE" — unsaved-copy.ts already renames it back on the way out); CONFLICT.*
// and PROVIDERS.DISCARD_AND_RELOAD stay in workspace-providers-copy.ts's
// PROVIDERS/PROVIDERS_DRAFT under their own, differently-spelled names
// (SAVED_ELSEWHERE_TITLE/BODY, CONFLICT_COPY/CONFLICT_COPIED_TOAST). Because
// the doc key and the module's own key diverge for several rows, this suite
// renders from an explicit map (the governance-copy.test.ts precedent) rather
// than a name-matching lookup.
const DOC = resolve(process.cwd(), "../docs/design/unsaved-guard-canon.md");

const doc = parseFrozenTables(DOC, /^## 1\. Frozen strings/);

const rendered: Record<string, string> = {
  "UNSAVED.DIRTY_CHIP": UNSAVED.DIRTY_CHIP,
  "UNSAVED.TITLE": UNSAVED.TITLE,
  "UNSAVED.BODY": UNSAVED.BODY,
  "UNSAVED.STAY": UNSAVED.STAY,
  "UNSAVED.DISCARD": UNSAVED.DISCARD,
  "CONFLICT.TITLE": PROVIDERS.SAVED_ELSEWHERE_TITLE,
  "CONFLICT.BODY": PROVIDERS.SAVED_ELSEWHERE_BODY,
  "CONFLICT.COPY_MINE": PROVIDERS_DRAFT.CONFLICT_COPY,
  "CONFLICT.COPIED_TOAST": PROVIDERS_DRAFT.CONFLICT_COPIED_TOAST,
  "PROVIDERS.DISCARD_AND_RELOAD": PROVIDERS_DRAFT.DISCARD_AND_RELOAD,
  "NAV.SETTINGS": NAV.SETTINGS,
};

describe("unsaved-guard-copy — unsaved-guard-canon.md, #495", () => {
  it("finds all 11 frozen keys in the doc", () => {
    expect(doc.size).toBe(11);
  });

  it.each(Object.keys(rendered))("%s is byte-exact", (key) => {
    expect(rendered[key]).toBe(doc.get(key));
  });

  it("covers every doc key, and freezes no key the doc doesn't", () => {
    expect(Object.keys(rendered).sort()).toEqual([...doc.keys()].sort());
  });
});
