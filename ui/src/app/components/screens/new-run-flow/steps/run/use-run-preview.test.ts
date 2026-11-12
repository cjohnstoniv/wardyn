/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Run step recognises the inbound feature refusal by the envelope's `feature`, never by its sentence: the wire
// reason capability_feature is shared by the feature doors, and a copy change must not silently turn the alert off.
import { describe, expect, it } from "vitest";
import { HttpError } from "../../../../../lib/api/core";
import { isInboundRefusal, previewRefusal, serviceRefusalOf } from "./use-run-preview";

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

// The window's clear keys on the envelope's field, so the preview hands it the field and the sentence as sent.
describe("previewRefusal", () => {
  it("carries the field and the sentence of an HttpError, and nothing else", () => {
    const e = new HttpError(403, "Opening a window onto a macOS VM run is turned off for this install.", "mac_window_site_off", "", "", "", undefined, "", "", "desktop_window");
    expect(previewRefusal(e)).toEqual({ field: "desktop_window", text: "Opening a window onto a macOS VM run is turned off for this install." });
    expect(previewRefusal(new HttpError(403, "x", "governance_unit"))).toEqual({ field: "", text: "x" });
    expect(previewRefusal(new Error("x"))).toBeNull();
    expect(previewRefusal(null)).toBeNull();
  });
});

describe("serviceRefusalOf", () => {
  const invalid = (message: string, field: string) => new HttpError(400, message, "run_mode_invalid", "", "", "", undefined, "", "", field);

  it("is the server's sentence, raw, for a refused service field", () => {
    expect(serviceRefusalOf(true, invalid('service.health.probe: invalid "tcp"', "service.health.probe"))).toBe('service.health.probe: invalid "tcp"');
  });

  it("is nothing for another field, another reason, or a draft with no service", () => {
    expect(serviceRefusalOf(true, invalid("x", "machine_kind"))).toBeUndefined();
    expect(serviceRefusalOf(true, invalid("x", ""))).toBeUndefined();
    expect(serviceRefusalOf(false, invalid("x", "service.health.probe"))).toBeUndefined();
    expect(serviceRefusalOf(true, new HttpError(422, "x", "run_service_shape", "", "", "", undefined, "", "", "service.health.probe"))).toBeUndefined();
    expect(serviceRefusalOf(true, new Error("x"))).toBeUndefined();
  });
});
