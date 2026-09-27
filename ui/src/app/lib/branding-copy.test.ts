/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { parseFrozenTables, renderFromNamespaces } from "./copy-doc-parity";
import { BRAND_HEADER, BRAND_NAME, BRANDING } from "./branding-copy";

// docs/design/branding-canon.md (#1125) parsed back out and compared whole: a
// changed string, a new doc row or a dropped one all fail here. process.cwd()
// is the vitest root, ui/.
const doc = parseFrozenTables(
  resolve(process.cwd(), "../docs/design/branding-canon.md"),
  /^## (Frozen|Implementation) strings/,
);

const rendered = Object.fromEntries(
  [...doc.keys()].map((key) => {
    const [ns, rest] = key.split(/\.(.*)/s);
    const space = ({ BRAND_NAME, BRAND_HEADER, BRANDING } as Record<string, Record<string, unknown>>)[ns] ?? {};
    return [key, renderFromNamespaces(rest, [space])];
  }),
);

describe("branding-copy — docs/design/branding-canon.md", () => {
  it("renders every frozen string byte for byte", () => {
    expect(rendered).toEqual(Object.fromEntries(doc));
  });

  it("has no string the doc lacks", () => {
    const keys = [
      ...Object.keys(BRAND_NAME).map((k) => `BRAND_NAME.${k}`),
      ...Object.keys(BRAND_HEADER).map((k) => `BRAND_HEADER.${k}`),
      ...Object.keys(BRANDING).map((k) => `BRANDING.${k}`),
    ].sort();
    const docKeys = [...doc.keys()].map((k) => k.replace(/\(.*\)$/, "")).sort();
    expect(keys).toEqual(docKeys);
  });

  it("keeps the packet's no-break space in 512 KB", () => {
    expect(BRANDING.LOGO_HINT).toContain("512 KB");
    expect(BRANDING.ERR_LOGO("3.6 MB")).toBe("This logo is 3.6 MB. Upload an image under 512 KB (SVG or PNG).");
  });
});
