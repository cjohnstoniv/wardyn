/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { installLines, orgCAFingerprints } from "./cli-install";

const REL = "https://github.com/cjohnstoniv/wardyn/releases/download/v0.10.0";

describe("installLines", () => {
  it("is the release's two lines, byte for byte, when no mirror is set", () => {
    expect(installLines("0.10.0", undefined)).toEqual({
      version: "0.10.0",
      sh: `curl -fsSL ${REL}/install-cli.sh | sh`,
      ps: `irm ${REL}/install-cli.ps1 | iex`,
      mirror: false,
    });
    expect(installLines("v0.10.0", "")).toEqual(installLines("0.10.0", undefined));
  });

  // The mirror is used twice per line: the script's own URL and WARDYN_ARTIFACT_BASE (§11.14 answer 1).
  it("uses the advertised mirror for the script URL and for WARDYN_ARTIFACT_BASE, in both forms", () => {
    const b = "https://artifacts.example.com/wardyn";
    expect(installLines("0.10.0", b)).toEqual({
      version: "0.10.0",
      sh: `curl -fsSL ${b}/v0.10.0/install-cli.sh | WARDYN_ARTIFACT_BASE=${b} sh`,
      ps: `$env:WARDYN_ARTIFACT_BASE='${b}'; irm ${b}/v0.10.0/install-cli.ps1 | iex`,
      mirror: true,
    });
    expect(installLines("0.10.0", `${b}/`)).toEqual(installLines("0.10.0", b));
  });

  it("names no release for a missing version or a source build", () => {
    expect(installLines(undefined, undefined)).toBeNull();
    expect(installLines("", undefined)).toBeNull();
    expect(installLines("0.10.0+9f3c1ab2d-dirty", undefined)).toBeNull();
    expect(installLines("dev", undefined)).toBeNull();
  });

  it("keeps a pre-release suffix", () => {
    expect(installLines("0.10.0-rc1", undefined)?.sh).toContain("/v0.10.0-rc1/install-cli.sh");
  });

  it("never draws a mirror that is not a plain https URL into a shell line", () => {
    for (const bad of ["http://a.example/x", "https://a.example/x';rm -rf ~;'", "https://a.example/$(id)", "https://a.example/ x", "ftp://a"]) {
      expect(installLines("0.10.0", bad)).toBeNull();
    }
  });
});

describe("orgCAFingerprints", () => {
  it("keeps the order, reads a string or an object, and labels with the subject", () => {
    expect(
      orgCAFingerprints(["sha256:aa", { subject: "Corp Root", sha256: "sha256:bb" }, { fingerprint: "sha256:cc" }]),
    ).toEqual([{ fingerprint: "sha256:aa" }, { fingerprint: "sha256:bb", subject: "Corp Root" }, { fingerprint: "sha256:cc" }]);
  });

  it("is empty for anything else", () => {
    for (const v of [undefined, null, "sha256:aa", {}, [], [""], [{}], [3]]) expect(orgCAFingerprints(v)).toEqual([]);
  });
});
