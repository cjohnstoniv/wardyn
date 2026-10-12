/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run canon (docs/design/new-run-canon.md, section 7) is parsed back here and every
// row rendered from its screen's lazy copy module byte for byte, in both directions: a reworded
// constant, a doc row with no key, or a key with no doc row fails this suite. A parameterized key
// is called with its own placeholders, so USED_OF_MAX("{used}", "{max}") must reproduce the
// doc's "{used} of {max} running". `server` rows are pointers: the Go sentence owns them, so no
// module may carry a constant for one.
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { NR_DOCUMENT } from "../components/wardyn/copy/new-run/document";
import { NR_FILES } from "../components/wardyn/copy/new-run/files";
import { NR_HARNESSES } from "../components/wardyn/copy/new-run/harnesses";
import { NR_LAUNCH } from "../components/wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../components/wardyn/copy/new-run/review";
import { NR_RUN } from "../components/wardyn/copy/new-run/run";
import { NR_SAVE } from "../components/wardyn/copy/new-run/save";
import { NR_SHAPE } from "../components/wardyn/copy/new-run/shape";
import { NR_SHARED } from "../components/wardyn/copy/new-run/shared";
import { NR_START } from "../components/wardyn/copy/new-run/start";
import { NR_TEMPLATES } from "../components/wardyn/copy/new-run/templates";
import { NR_TOOLS } from "../components/wardyn/copy/new-run/tools";
import { parseFrozenTables, renderFromNamespaces, splitKey } from "./copy-doc-parity";

const DOC = resolve(process.cwd(), "../docs/design/new-run-canon.md");
const COPY_BARREL = resolve(process.cwd(), "src/app/components/wardyn/copy.ts");

// Sections 7.10 and 7.11 share NR_LAUNCH: the Name dialog and the Launched screen are one file.
const SCREENS: { name: string; sections: string[]; ns: Record<string, unknown> }[] = [
  { name: "Start", sections: ["7.1"], ns: NR_START },
  { name: "Shape", sections: ["7.2"], ns: NR_SHAPE },
  { name: "Run", sections: ["7.3"], ns: NR_RUN },
  { name: "Harnesses", sections: ["7.4"], ns: NR_HARNESSES },
  { name: "Files", sections: ["7.5"], ns: NR_FILES },
  { name: "Tools and access", sections: ["7.6"], ns: NR_TOOLS },
  { name: "Document", sections: ["7.7"], ns: NR_DOCUMENT },
  { name: "Review", sections: ["7.8"], ns: NR_REVIEW },
  { name: "Save as", sections: ["7.9"], ns: NR_SAVE },
  { name: "Name and launch, Launched", sections: ["7.10", "7.11"], ns: NR_LAUNCH },
  { name: "Templates", sections: ["7.12"], ns: NR_TEMPLATES },
  { name: "Shared", sections: ["7.13"], ns: NR_SHARED },
];

const headingOf = (sections: string[]) => new RegExp(`^### (${sections.map((s) => s.replace(".", "\\.")).join("|")}) `);

// Every `| Key | Source | Frozen string |` row of section 7 with its section and source cell.
function docRows(): { section: string; key: string; source: string }[] {
  const rows: { section: string; key: string; source: string }[] = [];
  let section = "";
  for (const line of readFileSync(DOC, "utf8").split("\n")) {
    if (line.startsWith("#")) {
      section = /^### (7\.\d+) /.exec(line)?.[1] ?? "";
      continue;
    }
    if (!section || !line.startsWith("|")) continue;
    const cells = line.split("|").slice(1, -1).map((c) => c.trim());
    if (cells[0] === "Key" || /^:?-+:?$/.test(cells[0])) continue;
    rows.push({ section, key: cells[0].replace(/`/g, ""), source: cells[1] });
  }
  return rows;
}

const ROWS = docRows();
const isServer = (source: string) => source === "server";

describe("new run canon: rows", () => {
  it("every row names a known source, and a deviation or pending row cites its table", () => {
    const bad = ROWS.filter((r) => !/^(mock|server|deviation \(.+\)|pending \((W-\d+|A\d+)\))$/.test(r.source));
    expect(bad.map((r) => `${r.section} ${r.key}: ${r.source}`)).toEqual([]);
  });

  it("every section 7 table belongs to a screen", () => {
    const mapped = new Set(SCREENS.flatMap((s) => s.sections));
    expect([...new Set(ROWS.map((r) => r.section))].filter((s) => !mapped.has(s))).toEqual([]);
    expect(mapped.size).toBe(13);
  });
});

for (const { name, sections, ns } of SCREENS) {
  describe(`new run canon: ${name}`, () => {
    const rows = ROWS.filter((r) => sections.includes(r.section));
    const frozen = rows.filter((r) => !isServer(r.source));
    const doc = parseFrozenTables(DOC, headingOf(sections));

    it("finds the frozen tables, with no row repeated", () => {
      expect(rows.length).toBeGreaterThan(0);
      expect(doc.size).toBe(rows.length);
    });

    it("covers every frozen doc key, and freezes no key the doc doesn't", () => {
      expect(Object.keys(ns).sort()).toEqual(frozen.map((r) => splitKey(r.key)[0]).sort());
    });

    it("keeps a server row's sentence out of the module", () => {
      for (const r of rows.filter((r) => isServer(r.source))) expect(splitKey(r.key)[0] in ns).toBe(false);
    });

    it.each(frozen.map((r) => r.key))("%s is byte-exact", (key) => {
      expect(renderFromNamespaces(key, [ns])).toBe(doc.get(key));
    });
  });
}

// "- `NR_TOOLS.KEY` shows only when ..." lines in the doc's Display rules name constants a screen must gate.
describe("new run canon: display rules", () => {
  const named = [...readFileSync(DOC, "utf8").matchAll(/^- `(NR_[A-Z]+)\.([A-Z0-9_]+)` /gm)].map((m) => [m[1], m[2]]);

  it("names constants that exist", () => {
    expect(named.length).toBeGreaterThan(0);
    const all: Record<string, Record<string, unknown>> = {
      NR_TOOLS,
      NR_START,
      NR_SHAPE,
      NR_RUN,
      NR_HARNESSES,
      NR_FILES,
      NR_DOCUMENT,
      NR_REVIEW,
      NR_SAVE,
      NR_LAUNCH,
      NR_TEMPLATES,
      NR_SHARED,
    };
    expect(named.filter(([ns, key]) => !(key in (all[ns] ?? {})))).toEqual([]);
  });
});

describe("new run copy is lazy-only", () => {
  it("is not re-exported through the eager copy.ts barrel", () => {
    expect(readFileSync(COPY_BARREL, "utf8")).not.toMatch(/copy\/new-run\//);
  });
});
