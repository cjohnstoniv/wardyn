/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// PUSH.RAIL_BODY's pluralization, and the drift guard on the ApprovalKindChip
// path: copy/approvals.ts hand-copies "Push" (eager-bundle reasons — see its
// own comment) rather than importing PUSH.KIND_LABEL, so the two strings can
// only ever be proven equal by a test, not the type system.
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it, expect } from "vitest";
import { PUSH } from "./push";
import { APPROVAL_KIND_LABEL } from "./approvals";

describe("PUSH copy (#181)", () => {
  it("pluralizes RAIL_BODY correctly for 0, 1 and many", () => {
    expect(PUSH.RAIL_BODY(0, 0)).toBe("0 paths denied · 0 paths held for review");
    expect(PUSH.RAIL_BODY(1, 1)).toBe("1 path denied · 1 path held for review");
    expect(PUSH.RAIL_BODY(2, 3)).toBe("2 paths denied · 3 paths held for review");
    expect(PUSH.RAIL_BODY(1, 2)).toBe("1 path denied · 2 paths held for review");
  });

  it("PATHS_MORE reads '+N more'", () => {
    expect(PUSH.PATHS_MORE(4)).toBe("+4 more");
  });

  it("never drifts from copy/approvals.ts's hand-copied kind label", () => {
    expect(APPROVAL_KIND_LABEL.push).toBe(PUSH.KIND_LABEL);
  });

  // SD-7: the shipped HELD_OPEN sentence diverged from the mock the owner
  // actually froze (packet 7b, Q7b-1) — a different key name (HELD_EXPIRED)
  // and a shorter sentence. Hardcoded against the mock's own bytes
  // (wardyn-archive/mock-08/packet-7b.html), not derived from the module
  // under test, so a future edit to push.ts can't silently drag this along.
  it("HELD_EXPIRED matches the frozen mock sentence exactly (packet 7b, Q7b-1)", () => {
    expect(PUSH.HELD_EXPIRED).toBe(
      "No longer waiting — approving lets the next push of these same commits through.",
    );
  });
});

// The canon doc's PUSH rows (frozen and DRAFT), parsed back and compared row
// for row: a reworded clause, a swapped dash or a row added on one side only
// fails here. A key's argument names are the doc's own placeholders, so
// `PUSH.WHAT(repo, person)` is checked as PUSH.WHAT("{repo}", "{person}").
const DOC = resolve(process.cwd(), "../docs/design/held-push-canon.md");

function docRows(): Record<string, string> {
  const rows: Record<string, string> = {};
  for (const line of readFileSync(DOC, "utf8").split("\n")) {
    const cells = line.split("|").slice(1, -1).map((c) => c.trim());
    const key = cells[0]?.replace(/`/g, "");
    if (cells.length >= 2 && key?.startsWith("PUSH.")) rows[key] = cells[1];
  }
  return rows;
}

function moduleRows(): Record<string, string> {
  const doc = Object.keys(docRows());
  const rows: Record<string, string> = {};
  for (const [name, value] of Object.entries(PUSH)) {
    if (typeof value === "string") {
      rows[`PUSH.${name}`] = value;
      continue;
    }
    const key = doc.find((k) => k.startsWith(`PUSH.${name}(`)) ?? `PUSH.${name}(?)`;
    const args = key.slice(key.indexOf("(") + 1, -1).split(",").map((a) => `{${a.trim()}}`);
    rows[key] = (value as (...a: string[]) => string)(...args);
  }
  return rows;
}

describe("copy/push.ts matches docs/design/held-push-canon.md", () => {
  it("every PUSH row, byte for byte, and nothing extra on either side", () => {
    expect(moduleRows()).toEqual(docRows());
  });
});
