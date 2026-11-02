/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Run step recognises the inbound feature refusal by the envelope's `feature`, never by its sentence: the wire
// reason capability_feature is shared by the feature doors, and a copy change must not silently turn the alert off.
import { describe, expect, it } from "vitest";
import { HttpError } from "../../../../../lib/api/core";
import { isInboundRefusal } from "./use-run-preview";

const refusal = (message: string, feature?: string) => new HttpError(403, message, "capability_feature", "", "", "", undefined, "", "", "", feature);

describe("isInboundRefusal", () => {
  it("follows the feature, not the sentence", () => {
    expect(isInboundRefusal(true, refusal("Inbound apps aren't turned on for you. Ask your admin.", "inbound"))).toBe(true);
    expect(isInboundRefusal(true, refusal("Public ports are off for you.", "inbound"))).toBe(true);
    expect(isInboundRefusal(true, refusal("Inbound apps aren't turned on for you. Ask your admin."))).toBe(false);
    expect(isInboundRefusal(true, refusal("Custom components aren't turned on for you.", "custom_component"))).toBe(false);
  });

  it("needs a declared inbound app, the capability_feature reason and an HttpError", () => {
    expect(isInboundRefusal(false, refusal("x", "inbound"))).toBe(false);
    expect(isInboundRefusal(true, new HttpError(403, "x", "capability_component", "", "", "", undefined, "", "", "", "inbound"))).toBe(false);
    expect(isInboundRefusal(true, new Error("x"))).toBe(false);
    expect(isInboundRefusal(true, null)).toBe(false);
  });
});
