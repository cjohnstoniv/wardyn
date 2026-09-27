/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import {
  contrastRatio,
  DARK_BACKGROUND,
  deriveDarkPrimary,
  isHexColour,
  logoSizeText,
  monogram,
  productName,
  ratioText,
} from "./branding";

describe("branding helpers (#1125)", () => {
  it("names the product in both formats, and Wardyn unbranded", () => {
    expect(productName({ org_name: "Example Corp", name_format: "prefix" })).toBe("Example Corp Wardyn");
    expect(productName({ org_name: "Example Corp", name_format: "suffix" })).toBe("Wardyn for Example Corp");
    expect(productName({})).toBe("Wardyn");
    expect(productName(null)).toBe("Wardyn");
  });

  it("checks hex colours the way the server does", () => {
    for (const ok of ["#0f766e", "#FFF", "#7c3aed"]) expect(isHexColour(ok)).toBe(true);
    for (const bad of ["#7Q3aeZ", "0f766e", "#12345", "white", ""]) expect(isHexColour(bad)).toBe(false);
  });

  it("computes WCAG contrast, rounded down for display", () => {
    expect(ratioText(contrastRatio("#ffffff", "#000000"))).toBe("21.0");
    expect(ratioText(contrastRatio("#fef08a", "#ffffff"))).toBe("1.1");
    expect(ratioText(4.46)).toBe("4.4");
    expect(contrastRatio("#7c3aed", "#ffffff")).toBeGreaterThanOrEqual(4.5);
  });

  // The same vectors internal/api/branding_test.go's TestDeriveDarkPrimaryVectors
  // pins, so the console's preview and the server's answer cannot drift.
  it("derives the dark pair exactly as the server does, always passing", () => {
    const vectors: Record<string, string> = {
      "#7c3aed": "#9058f0",
      "#0f766e": "#338b84",
      "#000000": "#808080",
      "#fef08a": "#fef08a",
      "#b91c1c": "#cb5555",
    };
    for (const [light, dark] of Object.entries(vectors)) {
      const d = deriveDarkPrimary(light);
      expect(d).toEqual({ primary: dark, text: DARK_BACKGROUND });
      expect(contrastRatio(d.primary, d.text)).toBeGreaterThanOrEqual(4.5);
    }
  });

  it("says a size the way the server does, and a monogram from the name", () => {
    expect(logoSizeText(18400)).toBe("18 KB");
    expect(logoSizeText(512 * 1024 + 1)).toBe("513 KB");
    expect(logoSizeText(3_560_000)).toBe("3.6 MB");
    expect(monogram("Example Corp")).toBe("EC");
    expect(monogram("acme")).toBe("A");
  });
});
