import { describe, expect, it } from "vitest";
import { nonNegativeInt } from "./format";

// A limit field's zero means "no limit"; nothing a number input can emit may
// become a negative or fractional cap that the server would read as unlimited
// while the editor shows a number.
describe("nonNegativeInt", () => {
  it("keeps positive integers and truncates fractions", () => {
    expect(nonNegativeInt("4096")).toBe(4096);
    expect(nonNegativeInt("12.9")).toBe(12);
  });
  it("collapses blank, NaN, negative and zero to 0", () => {
    for (const raw of ["", "abc", "-5", "-0.1", "0", "NaN", "Infinity"]) {
      expect(nonNegativeInt(raw)).toBe(0);
    }
  });
});
