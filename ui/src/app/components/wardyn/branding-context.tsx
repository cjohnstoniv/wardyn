/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Console branding (#1125): one anonymous read of the org's brand, shared by
// the sign-in page, the top bar and the Branding card. Unbranded (no record, a
// failed read, or no provider at all, as in a unit test) is a brand with no
// org_name, and every BrandSlot then renders its fallback — exactly what that place rendered
// before branding existed.
//
// Only this read is in the entry chunk, which sits at its size budget
// (bundle-split.test.ts, #498): everything that draws a brand is branded.tsx,
// loaded once a brand is set.
import * as React from "react";
import { wfetch } from "../../lib/api/core";
import type { Branding } from "../../lib/branding";
import type { Part } from "./branded";

/** [brand, setBrand] — the Branding card sets what it just saved. */
export const BrandingContext = React.createContext<[Branding, (b: Branding) => void]>([{}, () => {}]);

const Branded = React.lazy(() => import("./branded"));

/** A brand's part where one is set; `fallback` otherwise, and while branded.tsx loads. */
export function BrandSlot({ part, fallback }: { part: Part; fallback?: React.ReactNode }) {
  const [brand] = React.useContext(BrandingContext);
  if (!brand.org_name) return fallback;
  return (
    <React.Suspense fallback={fallback}>
      <Branded part={part} brand={brand} />
    </React.Suspense>
  );
}

export function BrandingProvider({ children }: { children: React.ReactNode }) {
  const [brand, setBrand] = React.useState<Branding>({});
  React.useEffect(() => {
    // An error answer ({"error": …}) has no org_name either: unbranded, like
    // no answer at all.
    wfetch("/branding")
      .then((r) => r.json() as Promise<Branding>)
      .then(setBrand)
      .catch(() => {});
  }, []);
  // A fresh pair only when the brand changes: this provider sits above App
  // (main.tsx) and re-renders for nothing else.
  return (
    <BrandingContext.Provider value={[brand, setBrand]}>
      <BrandSlot part="head" />
      {children}
    </BrandingContext.Provider>
  );
}
