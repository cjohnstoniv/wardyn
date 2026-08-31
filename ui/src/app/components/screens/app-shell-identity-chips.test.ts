/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { isCustomTrustDomain, isCustomIdentityProvider } from "./app-shell";

// The chrome shows the trust domain and identity provider ONLY where they carry
// information. A default install always reports wardyn.local / embedded
// (internal/identity/embedded's DefaultTrustDomain and the identity registry's
// default), so surfacing them there is two constants nobody can act on, on every
// screen. The loading placeholder and the error value are quiet for the same
// reason: a chip reading "unknown" is worse than no chip.
describe("identity chrome — shown only when non-default", () => {
  it("stays quiet on a default install", () => {
    expect(isCustomTrustDomain("wardyn.local")).toBe(false);
    expect(isCustomIdentityProvider("embedded")).toBe(false);
  });

  it("stays quiet while loading, and on an unreachable daemon", () => {
    for (const v of ["", "…", "unknown"]) {
      expect(isCustomTrustDomain(v)).toBe(false);
      expect(isCustomIdentityProvider(v)).toBe(false);
    }
  });

  it("speaks up for a custom trust domain or an external provider", () => {
    expect(isCustomTrustDomain("corp.example.com")).toBe(true);
    expect(isCustomIdentityProvider("spire")).toBe(true);
  });
});
