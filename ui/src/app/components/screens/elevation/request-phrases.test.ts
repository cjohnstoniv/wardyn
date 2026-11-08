/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { decidersPhrase, joinOr, lengthWords } from "./request-phrases";

describe("joinOr", () => {
  it("joins with or, and a comma before the last only past two", () => {
    expect(joinOr([])).toBe("");
    expect(joinOr(["a"])).toBe("a");
    expect(joinOr(["a", "b"])).toBe("a or b");
    expect(joinOr(["a", "b", "c"])).toBe("a, b or c");
  });
});

describe("decidersPhrase", () => {
  it("names each kind of decider", () => {
    expect(decidersPhrase([{ kind: "security" }])).toBe("a security admin");
    expect(decidersPhrase([{ kind: "unit_admin", unit_name: "Payments" }])).toBe("admins of Payments");
    expect(decidersPhrase([{ kind: "unit_admin" }])).toBe("admins of this unit");
    expect(decidersPhrase([{ kind: "group", name: "support-leads" }])).toBe("the support-leads group");
  });

  it("joins two and three with or", () => {
    expect(decidersPhrase([{ kind: "unit_admin", unit_name: "Payments" }, { kind: "security" }])).toBe(
      "admins of Payments or a security admin",
    );
    expect(
      decidersPhrase([{ kind: "unit_admin", unit_name: "Payments" }, { kind: "group", name: "support-leads" }, { kind: "security" }]),
    ).toBe("admins of Payments, the support-leads group or a security admin");
  });
});

describe("lengthWords", () => {
  it("writes a stored duration in words", () => {
    expect(lengthWords("4h")).toBe("4 hours");
    expect(lengthWords("30m")).toBe("30 minutes");
    expect(lengthWords("1h30m")).toBe("90 minutes");
  });
  it("hands back what it cannot read", () => {
    expect(lengthWords("soon")).toBe("soon");
  });
});
