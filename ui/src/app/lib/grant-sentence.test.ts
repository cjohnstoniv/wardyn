/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { grantSentence, grantsWrite, scopeRepos } from "./grant-sentence";

describe("grantSentence", () => {
  it("reads a read-only GitHub token with no repositories as words, not JSON", () => {
    expect(grantSentence("github_token", { repos: [], permissions: { contents: "read" } })).toBe(
      "GitHub token · read contents · no repositories listed",
    );
  });

  it("names the repositories and every permission", () => {
    expect(
      grantSentence("github_token", { repos: ["acme/api", "acme/web"], permissions: { contents: "write", pull_requests: "read" } }),
    ).toBe("GitHub token · write contents, read pull requests · acme/api, acme/web");
  });

  it("names the host of a git token or SSH key, and keeps an unknown kind's own word", () => {
    expect(grantSentence("git_pat", { host: "dev.azure.com" })).toBe("Git access token · dev.azure.com");
    expect(grantSentence("ssh_key", { host: "git.example.com" })).toBe("SSH key · git.example.com");
    expect(grantSentence("env_secret", {})).toBe("Environment secret");
    expect(grantSentence("future_kind", {})).toBe("future_kind");
  });

  it("never prints the scope's braces or quotes", () => {
    expect(grantSentence("github_token", { repos: [], permissions: { contents: "read" } })).not.toMatch(/[{}"[\]]/);
  });
});

describe("grantsWrite and scopeRepos", () => {
  it("flags any write, admin, push or maintain permission", () => {
    expect(grantsWrite({ permissions: { contents: "read" } })).toBe(false);
    expect(grantsWrite({ permissions: { contents: "read", issues: "write" } })).toBe(true);
    expect(grantsWrite({})).toBe(false);
  });

  it("keeps only string repos", () => {
    expect(scopeRepos({ repos: ["a/b", 3, null] })).toEqual(["a/b"]);
    expect(scopeRepos({})).toEqual([]);
  });
});
