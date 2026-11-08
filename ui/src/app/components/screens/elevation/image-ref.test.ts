/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { changeTargetRef, shortDigest, splitImageRef } from "./image-ref";

const HEX = "4f2a91c3" + "0".repeat(56);

describe("splitImageRef", () => {
  it("splits the name from the digest", () => {
    expect(splitImageRef(`ghcr.io/acme/build-tools@sha256:${HEX}`)).toEqual({ name: "ghcr.io/acme/build-tools", digest: `sha256:${HEX}` });
  });
  it("a reference with no digest is all name", () => {
    expect(splitImageRef("ghcr.io/acme/build-tools:latest")).toEqual({ name: "ghcr.io/acme/build-tools:latest", digest: "" });
  });
});

describe("shortDigest", () => {
  it("keeps sha256: and the first 8 hex, then an ellipsis", () => {
    expect(shortDigest(`sha256:${HEX}`)).toBe("sha256:4f2a91c3…");
  });
  it("a digest of no digest is empty", () => {
    expect(shortDigest("")).toBe("");
  });
});

describe("changeTargetRef", () => {
  it("is the last path segment and the short digest", () => {
    expect(changeTargetRef(`ghcr.io/acme/build-tools@sha256:${HEX}`)).toBe("build-tools@sha256:4f2a91c3…");
  });
  it("a name with no path keeps the name", () => {
    expect(changeTargetRef(`tool@sha256:${HEX}`)).toBe("tool@sha256:4f2a91c3…");
  });
});
