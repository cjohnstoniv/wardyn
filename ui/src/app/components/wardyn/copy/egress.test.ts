/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { EGRESS } from "./egress";

// The card lists approved, recorded, scan-detected, clone and inherited hosts;
// its subtitle must not say every row was approved or proven by a recording.
describe("EGRESS.ALLOWED_HOSTS_SUBTITLE", () => {
  it("does not claim every row was approved or recorded", () => {
    expect(EGRESS.ALLOWED_HOSTS_SUBTITLE).not.toMatch(/nothing is here unless/i);
    expect(EGRESS.ALLOWED_HOSTS_SUBTITLE).toContain("Each row says where it came from.");
  });

  it("is what the Allowed hosts card renders", () => {
    const src = readFileSync(resolve(process.cwd(), "src/app/components/screens/workspace-detail/allowed-hosts-card.tsx"), "utf8");
    expect(src).toContain("subtitle={EGRESS.ALLOWED_HOSTS_SUBTITLE}");
  });
});
