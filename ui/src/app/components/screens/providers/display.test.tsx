/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// baseURLError is the CLIENT MIRROR of the server's base-URL gate
// (validateWorkspaceProviders + validateProviderHostForKind,
// internal/api/workspace_providers.go:230-310). A mirror that admits what the
// server 400s is worse than no mirror: the admin types the URL their browser
// gave them (`dev.azure.com/org/project` is exactly that), sees no hint, presses
// Save and meets a refusal the field could have said first.
//
// PARITY is the same literal list as the GO twin,
// internal/api/workspace_providers_test.go's TestBaseURLClientMirrorParity —
// same inputs, same kinds, same verdicts, same order. A rule ported to one side
// and not the other reds the other side. Keep the two lists in lockstep.
import { describe, it, expect } from "vitest";
import type { GitProviderKind } from "../../../lib/api/providers";
import { baseURLError, invalidBaseURLLines } from "./display";

const PARITY: [url: string, kind: GitProviderKind, refused: boolean][] = [
  // github.com: an optional single /<org>, never a repo path.
  ["https://github.com", "github", false],
  ["https://github.com/acme", "github", false],
  ["https://github.com/acme/repo", "github", true],
  // A doubled slash is one segment (normalizeProviderBaseURL rebuilds the path).
  ["https://github.com//acme", "github", false],
  // dev.azure.com: the org segment is REQUIRED, and it is the ONLY one.
  ["https://dev.azure.com/acme", "azure_devops", false],
  ["https://dev.azure.com", "azure_devops", true],
  ["https://dev.azure.com/acme/proj", "azure_devops", true],
  // kind x host: the two well-known hosts belong to exactly one kind each.
  ["https://github.com/acme", "azure_devops", true],
  ["https://dev.azure.com/acme", "github", true],
  ["https://acme.visualstudio.com", "github", true],
  ["https://acme.visualstudio.com", "azure_devops", false],
  // Any OTHER host is accepted for either kind, with an OPTIONAL path (Q12).
  ["https://git.corp.example", "github", false],
  ["https://git.corp.example/acme/team", "github", false],
  ["https://git.corp.example/a/b/c", "github", true],
  ["https://tfs.corp.example/acme", "azure_devops", false],
  // Shape: https only, no userinfo, no port, no query or fragment, a dotted
  // host, and no percent-escape in the path (it hides a second segment).
  ["http://github.com/acme", "github", true],
  ["https://user:pw@github.com/acme", "github", true],
  ["https://github.com:8443/acme", "github", true],
  ["https://github.com/acme?x=1", "github", true],
  ["https://github.com/acme#frag", "github", true],
  ["https://localhost/acme", "github", true],
  ["https://github.com/acme%2Fevil", "github", true],
  ["https://github.com/%60id%60", "github", true],
  ["not a url", "github", true],
];

describe("baseURLError — parity with the server validator (Go twin: TestBaseURLClientMirrorParity)", () => {
  it.each(PARITY)("%s (%s) refused=%s", (url, kind, refused) => {
    expect(baseURLError(url, kind) !== null).toBe(refused);
  });

  // A blank line is not a refusal: a textarea mid-edit is routinely empty, and
  // an empty row is the server's own "base_urls is required" 400, not a
  // per-line shape complaint.
  it("says nothing about a blank line", () => {
    expect(baseURLError("", "github")).toBeNull();
    expect(baseURLError("   ", "azure_devops")).toBeNull();
    expect(invalidBaseURLLines("\n  \n", "github")).toEqual([]);
  });

  it("flags only the failing lines of a textarea", () => {
    expect(invalidBaseURLLines("https://github.com/acme\nhttps://github.com/acme/repo", "github")).toEqual([
      "https://github.com/acme/repo",
    ]);
  });
});
