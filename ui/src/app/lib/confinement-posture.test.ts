/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { resolveConfinementPosture } from "./confinement-posture";

// Issue #162's mock-approval comment table, verbatim — the premise correction
// that made this resolver take `runner` as well as `network_policy`: one
// empty `network_policy` covers Docker (not applicable) AND a k8s daemon that
// could not confirm (must warn), and the two need opposite treatment.
describe("resolveConfinementPosture", () => {
  it("docker, absent network_policy: not applicable — silent", () => {
    expect(resolveConfinementPosture("docker", "")).toBe("");
  });

  it("k8s, enforced: silent (nothing to warn about)", () => {
    expect(resolveConfinementPosture("k8s", "enforced")).toBe("enforced");
  });

  it("k8s, acknowledged: warns", () => {
    expect(resolveConfinementPosture("k8s", "acknowledged")).toBe("acknowledged");
  });

  it("k8s, unenforced: warns (the danger case)", () => {
    expect(resolveConfinementPosture("k8s", "unenforced")).toBe("unenforced");
  });

  it("k8s, absent network_policy: warns — ruling 2, could not confirm is never silent", () => {
    expect(resolveConfinementPosture("k8s", "")).toBe("unknown");
  });

  it("an unresolved or unrecognised runner reads as not-applicable, never a warning", () => {
    // The pre-mount /healthz default, and any future substrate this resolver
    // doesn't know about yet — must never invent a posture ahead of a real
    // /healthz answer just because the field is momentarily empty.
    expect(resolveConfinementPosture("", "")).toBe("");
    expect(resolveConfinementPosture("orchestrator", "unenforced")).toBe("");
  });
});
