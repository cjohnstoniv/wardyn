/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { EGRESS } from "./egress";

describe("EGRESS.PROMOTED_SKIPPED", () => {
  it("counts approved and skipped hosts and names the skipped ones", () => {
    expect(EGRESS.PROMOTED_SKIPPED(3, ["a.example.com", "b.example.com"])).toBe(
      "Approved 3 hosts. Skipped 2 model-provider hosts: a.example.com, b.example.com",
    );
    expect(EGRESS.PROMOTED_SKIPPED(1, ["a.example.com"])).toBe("Approved 1 host. Skipped 1 model-provider host: a.example.com");
  });
});
