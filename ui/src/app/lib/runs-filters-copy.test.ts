/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// docs/design/runs-filters-canon.md section 1 is parsed back and every row rendered from the copy
// module byte for byte (the unsaved-guard precedent). "Show N runs" is a plural pair, so both the
// other form SHOW(n) and the one form SHOW_ONE are rows; the test also checks the pair is used the
// way the doc says (1 reads the one form).
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { RUNS_FILTERS, RUNS_FILTERS_SHEET as S, RUNS_SEGMENTS as G } from "../components/wardyn/copy/runs-landing";
import { parseFrozenTables } from "./copy-doc-parity";

const doc = parseFrozenTables(resolve(process.cwd(), "../docs/design/runs-filters-canon.md"), /^## 1\. Frozen strings/);

const rendered: Record<string, string> = {
  TITLE: S.TITLE,
  "BUTTON_COUNT(n)": S.BUTTON_COUNT("{n}" as unknown as number),
  CLEAR: S.CLEAR,
  "SHOW(n)": S.SHOW("{n}" as unknown as number),
  SHOW_ONE: S.SHOW_ONE,
  "SHOW_MORE(n)": S.SHOW_MORE("{n}" as unknown as number),
  "CHIP_REMOVE_ARIA(chip)": S.CHIP_REMOVE_ARIA("{chip}"),
  "WORKSPACE_CHIP(w)": RUNS_FILTERS.WORKSPACE_CHIP("{w}"),
  SEGMENT_ALL: G.ALL,
  SEGMENT_AGENT_TASKS: G.AGENT_TASKS,
  SEGMENT_ENVIRONMENTS: G.ENVIRONMENTS,
  SEGMENT_APPS: G.APPS,
  SEGMENT_SERVICES: G.SERVICES,
  SEGMENT_SERVICES_EMPTY: G.SERVICES_EMPTY,
  SEGMENT_LABEL: G.LABEL,
};

describe("runs-filters-copy: runs-filters-canon.md, #3041", () => {
  it.each(Object.keys(rendered))("%s is byte-exact", (key) => {
    expect(rendered[key]).toBe(doc.get(key));
  });

  it("covers every doc key, and freezes no key the doc does not", () => {
    expect(Object.keys(rendered).sort()).toEqual([...doc.keys()].sort());
  });

  it("the plural pair: one run reads the one form, any other count the other form", () => {
    expect(S.SHOW_ONE).toBe("Show 1 run");
    expect(S.SHOW(9)).toBe("Show 9 runs");
  });
});
