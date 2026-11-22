/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A held change to a portal's admission rule or to an issuer's retirement reads in plain words in the changes
// list, not as its stored kind name.
import { describe, it, expect } from "vitest";
import { kindLabel } from "../../../lib/change-review";
import type { GovernanceChange } from "../../../lib/types";

const held = (target_kind: string) => ({ target_kind }) as GovernanceChange;

describe("the held portal change kinds", () => {
  it("name an admission change and an issuer retirement", () => {
    expect(kindLabel(held("delegate_admission"))).toBe("Portal admission change");
    expect(kindLabel(held("portal_issuer_retire"))).toBe("Portal issuer retirement");
  });
});
