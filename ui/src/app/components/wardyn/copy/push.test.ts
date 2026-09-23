/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// PUSH.RAIL_BODY's pluralization, and the drift guard on the ApprovalKindChip
// path: copy/approvals.ts hand-copies "Push" (eager-bundle reasons — see its
// own comment) rather than importing PUSH.KIND_LABEL, so the two strings can
// only ever be proven equal by a test, not the type system.
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
});
