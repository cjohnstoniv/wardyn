import { describe, expect, it } from "vitest";
import { cn } from "./utils";

describe("cn", () => {
  it.each(["text-meta", "text-body", "text-display"])(
    "keeps a %s size beside a text colour",
    (size) => {
      expect(cn("text-danger", size)).toBe(`text-danger ${size}`);
      expect(cn(size, "text-danger")).toBe(`${size} text-danger`);
    },
  );

  it("still lets a later size replace an earlier one", () => {
    expect(cn("text-xs", "text-meta")).toBe("text-meta");
    expect(cn("text-meta", "text-sm")).toBe("text-sm");
  });
});
