/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Console branding (#1125): the brand's shape, the product name, and the
// Branding card's live checks — hex colours, contrast, the dark-pair
// derivation, file sizes. The server (internal/api/branding_colour.go) runs
// the same checks on save and is what decides. deriveDarkPrimary is the
// server's walk, pinned to the same vectors on both sides. Nothing here is in
// the entry chunk (only the Branding type is): a brand is drawn by branded.tsx,
// loaded once a brand is set.
import { BRAND_NAME } from "./branding-copy";

export type NameFormat = "prefix" | "suffix";

/** What GET /branding (and /branding/settings) answer; `{}` when unbranded. */
export interface Branding {
  org_name?: string;
  name_format?: NameFormat;
  primary?: string;
  primary_text?: string;
  dark_primary?: string;
  dark_primary_text?: string;
  logo_url?: string;
  /** The tab icon: the logo, or the server-drawn monogram when there is none. */
  icon_url?: string;
  // /branding/settings only.
  support_url?: string;
  dark_custom?: boolean;
}

/** The product name a brand shows, or "Wardyn" unbranded. */
export function productName(b: Branding | null): string {
  if (!b?.org_name) return "Wardyn";
  return b.name_format === "suffix" ? BRAND_NAME.SUFFIX(b.org_name) : BRAND_NAME.PREFIX(b.org_name);
}

export const MIN_CONTRAST = 4.5;
export const LOGO_MAX_BYTES = 512 * 1024;
/** theme.css's .dark --background. */
export const DARK_BACKGROUND = "#0a0a0a";

type RGB = [number, number, number];

/** #rgb or #rrggbb, either case. */
export function isHexColour(v: string): boolean {
  return /^#([0-9a-fA-F]{6}|[0-9a-fA-F]{3})$/.test(v);
}

function toRGB(hex: string): RGB {
  let h = hex.slice(1);
  if (h.length === 3) h = h.replace(/./g, (c) => c + c);
  const n = parseInt(h, 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}

function toHex([r, g, b]: RGB): string {
  return `#${[r, g, b].map((v) => v.toString(16).padStart(2, "0")).join("")}`;
}

function luminance(c: RGB): number {
  const [r, g, b] = c.map((v) => {
    const s = v / 255;
    return s <= 0.03928 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

/** WCAG 2 contrast ratio between two hex colours. */
export function contrastRatio(a: string, b: string): number {
  const la = luminance(toRGB(a)) + 0.05;
  const lb = luminance(toRGB(b)) + 0.05;
  return Math.max(la, lb) / Math.min(la, lb);
}

/** One decimal, rounded DOWN — a failing 4.46 never reads as "4.5". */
export function ratioText(r: number): string {
  return (Math.floor(r * 10) / 10).toFixed(1);
}

/** The dark pair used when none is set: mix toward white until 4.5:1 on the dark background. */
export function deriveDarkPrimary(light: string): { primary: string; text: string } {
  const c = toRGB(light);
  const bg = DARK_BACKGROUND;
  for (let i = 0; i <= 20; i++) {
    const t = i / 20;
    const hex = toHex(c.map((v) => Math.round(v + (255 - v) * t)) as RGB);
    if (contrastRatio(hex, bg) >= MIN_CONTRAST) return { primary: hex, text: bg };
  }
  return { primary: "#ffffff", text: bg };
}

/** A file size the way the Branding card and the server say it. */
export function logoSizeText(bytes: number): string {
  return bytes >= 1_000_000 ? `${(bytes / 1_000_000).toFixed(1)} MB` : `${Math.ceil(bytes / 1024)} KB`;
}

/** The monogram a brand without a logo shows: the name's initials, at most two. */
export function monogram(name: string): string {
  return name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((w) => w[0])
    .join("")
    .toUpperCase();
}
