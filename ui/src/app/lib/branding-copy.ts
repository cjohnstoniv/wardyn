/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Console branding (#1125) — frozen strings, byte for byte from the approved
// packet; docs/design/branding-canon.md is the table branding-copy.test.ts
// parses back out. "512\u00a0KB" carries the packet's no-break space.

export const BRAND_NAME = {
  PREFIX: (company: string) => `${company} Wardyn`,
  SUFFIX: (company: string) => `Wardyn for ${company}`,
} as const;

export const BRAND_HEADER = {
  SUPPORT_CHIP: "Support",
} as const;

export const BRANDING = {
  TITLE: "Branding",
  LEDE: "How this Wardyn console looks and introduces itself to the people who sign in.",
  ORG_NAME_LABEL: "Organisation name",
  NAME_FORMAT_LABEL: "How the product name appears",
  PRIMARY_LABEL: "Primary colour",
  TEXT_LABEL: "Text on primary",
  LOGO_LABEL: "Logo",
  LOGO_HINT:
    "SVG or PNG, up to 512 KB. Square works best — it appears at 28px in the header and 32px as the browser tab icon.",
  LINK_LABEL: "Support link (optional)",
  LINK_HINT: "Shown in the header. Must be https.",
  DARK_LABEL: "Use a different colour in dark mode (optional)",
  SAVE: "Save branding",
  PREVIEW_TITLE: "Live preview",
  ERR_COLOR: "Enter a valid hex colour, like #0f766e.",
  ERR_CONTRAST: (n: string) =>
    `This text colour has a contrast ratio of ${n}:1 against the button background. Wardyn requires at least 4.5:1 for body text (WCAG AA).`,
  ERR_LINK: "This link must use https. http:// links, and links with no scheme, aren't allowed.",
  ERR_LOGO: (size: string) => `This logo is ${size}. Upload an image under 512 KB (SVG or PNG).`,
  // Implementation strings (the canon doc's second table).
  FORMAT_PREFIX_HINT: "<Company> Wardyn",
  FORMAT_SUFFIX_HINT: "Wardyn for <Company>",
  CONTRAST_OK: (n: string) => `${n}:1 — passes WCAG AA.`,
  FIX_ONE: "Fix the highlighted field to save.",
  FIX_MANY: "Fix the highlighted fields to save.",
  FIXED_TITLE: "Fixed, never brandable",
  SAVED: "Branding saved.",
  SAVE_FAILED: "Branding wasn't saved.",
} as const;
