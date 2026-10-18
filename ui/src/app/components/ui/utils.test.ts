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

  // M1: these rungs set a size only, so a line-height passed beside them (Chip's leading-5,
  // Label's leading-none) must survive, as it did before #2590.
  it.each(["text-meta", "text-body", "text-display"])("keeps leading-* beside %s", (size) => {
    expect(cn("leading-5", size)).toBe(`leading-5 ${size}`);
    expect(cn(size, "leading-none")).toBe(`${size} leading-none`);
    expect(cn("leading-5", "text-danger", size)).toBe(`leading-5 text-danger ${size}`);
  });

  it("still drops leading-* for a built-in size, which carries its own line-height", () => {
    expect(cn("leading-5", "text-xs")).toBe("text-xs");
  });

  it("a token and another token replace each other", () => {
    expect(cn("text-meta", "text-body")).toBe("text-body");
  });
});
