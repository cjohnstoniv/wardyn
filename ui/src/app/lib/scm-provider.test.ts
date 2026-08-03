/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { CAPABILITY } from "../components/wardyn/copy";
import {
  slugHost,
  hostError,
  laneOfName,
  LANE_META,
  LEGACY_NAMES,
  deriveProviders,
} from "./scm-provider";

// Mirrors secrets.tsx:48's server-mirrored name policy. Duplicated rather
// than imported (that constant isn't exported) — the same mirroring pattern
// secrets.tsx itself uses one level down, against the Go regex.
const SECRET_NAME_RE = /^[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$/;

describe("slugHost", () => {
  it("trims, lowercases, and turns dots into hyphens", () => {
    expect(slugHost("  GHES.Corp.Internal  ")).toBe("ghes-corp-internal");
  });

  it("collapses runs of non [a-z0-9.] characters to a single hyphen", () => {
    expect(slugHost("dev.azure.com:8080/org")).toBe("dev-azure-com-8080-org");
  });

  it("collapses repeated hyphens and strips leading/trailing hyphens", () => {
    expect(slugHost("--git...server--")).toBe("git-server");
  });

  it("is not reversible when the host itself contains a hyphen (the ambiguity deriveProviders works around, not this function)", () => {
    const slug = slugHost("git-server.corp.com");
    expect(slug).toBe("git-server-corp-com");
    expect(slug.replace(/-/g, ".")).not.toBe("git-server.corp.com");
  });

  it("keeps a realistic host set's git-pat-<slug> name within SECRET_NAME_RE's 128-char cap", () => {
    const hosts = [
      "github.com",
      "dev.azure.com",
      "ghes.corp.internal",
      "git-server.corp.com",
      // Unicode + punctuation, as if pasted from a doc: curly apostrophe, an
      // em dash, doubled punctuation, and an accented letter.
      "Café's Git—Server!!  Internal.example.com",
      // Long, hostname-shaped (stands in for a generated/hashed build-agent
      // subdomain) — sized so "git-pat-" (8 chars) + slug lands EXACTLY at
      // the 128-char cap, not just comfortably under it.
      `${"b".repeat(108)}.example.com`,
    ];
    for (const host of hosts) {
      const name = `git-pat-${slugHost(host)}`;
      expect(name.length).toBeLessThanOrEqual(128);
      expect(SECRET_NAME_RE.test(name)).toBe(true);
    }
  });
});

describe("hostError", () => {
  it("accepts a bare dotted host and treats empty as 'not filled in yet'", () => {
    expect(hostError("ghes.corp.internal")).toBeNull();
    expect(hostError("  GHES.Corp.Internal ")).toBeNull();
    expect(hostError("")).toBeNull();
  });

  it("rejects what validSiteHost rejects — a dot is required, and no scheme/port/path", () => {
    // The exact case that used to save the secret and then 400 on Done.
    expect(hostError("localhost")).toMatch(/dot/i);
    expect(hostError("https://ghes.corp.internal/")).toMatch(/dot/i);
    expect(hostError("ghes.corp.internal:8080")).toMatch(/dot/i);
  });

  it("rejects a host whose derived secret name would exceed the 128-char store limit", () => {
    const long = `${"b".repeat(130)}.example.com`;
    expect(hostError(long)).toMatch(/128/);
    // The boundary case the slugHost test pins: exactly 128 is still fine.
    expect(hostError(`${"b".repeat(108)}.example.com`)).toBeNull();
    expect(`git-pat-${slugHost(`${"b".repeat(108)}.example.com`)}`.length).toBe(128);
  });
});

describe("laneOfName", () => {
  it("classifies github-app-* as app and ssh-key-* as ssh", () => {
    expect(laneOfName("github-app-id")).toBe("app");
    expect(laneOfName("ssh-key-github-com")).toBe("ssh");
  });

  it("defaults everything else to pat, including non-SCM secrets", () => {
    expect(laneOfName("git-pat-github-com")).toBe("pat");
    expect(laneOfName("anthropic-api-key")).toBe("pat");
  });
});

describe("LANE_META — honesty canon", () => {
  it("pins the tooltip copy verbatim, em-dashes included", () => {
    expect(LANE_META.app.tooltip).toBe(
      "The run works through a short-lived, scoped credential — your stored key stays in Wardyn.",
    );
    expect(LANE_META.pat.tooltip).toBe(
      "A git access token is handed to git inside the sandbox — the process running there can read it.",
    );
    expect(LANE_META.ssh.tooltip).toBe(
      "A private SSH key is written to disk in the sandbox — the process running there can read it.",
    );
  });

  it("reuses CAPABILITY's grant-honesty wording instead of a second copy of the same sentences", () => {
    expect(LANE_META.app.tooltip).toBe(CAPABILITY.brokerLine);
    expect(LANE_META.pat.tooltip).toBe(CAPABILITY.gitPatLine);
    expect(LANE_META.ssh.tooltip).toBe(CAPABILITY.sshKeyLine);
  });

  it("pins the chip labels verbatim (U+00B7 middle dot, not a hyphen)", () => {
    expect(LANE_META.app.label).toBe("App · brokered");
    expect(LANE_META.pat.label).toBe("PAT · in-sandbox");
    expect(LANE_META.ssh.label).toBe("SSH · resident");
  });

  it("pins each lane's chip tone", () => {
    expect(LANE_META.app.tone).toBe("success");
    expect(LANE_META.pat.tone).toBe("info");
    expect(LANE_META.ssh.tone).toBe("warning");
  });
});

describe("LEGACY_NAMES", () => {
  it("lists exactly the four pre-convention names", () => {
    expect(LEGACY_NAMES).toEqual(["github-pat", "gitlab-pat", "ado-pat", "bitbucket-pat"]);
  });

  it("never produces a deriveProviders row", () => {
    expect(deriveProviders([...LEGACY_NAMES], [], false)).toEqual([]);
  });
});

describe("deriveProviders", () => {
  it("seeds a zero-credential row for every scmHosts entry, sorted by host", () => {
    const rows = deriveProviders([], ["gitlab.com", "bitbucket.org"], false);
    expect(rows).toEqual([
      { host: "bitbucket.org", brand: "Bitbucket", lanes: [] },
      { host: "gitlab.com", brand: "GitLab", lanes: [] },
    ]);
  });

  it("maps the known brands and falls back to 'Git host'", () => {
    const rows = deriveProviders(
      [],
      [
        "github.com",
        "dev.azure.com",
        "gitlab.com",
        "bitbucket.org",
        "ghes.corp.internal",
        "some.other.host",
      ],
      false,
    );
    expect(Object.fromEntries(rows.map((r) => [r.host, r.brand]))).toEqual({
      "github.com": "GitHub",
      "dev.azure.com": "Azure DevOps",
      "gitlab.com": "GitLab",
      "bitbucket.org": "Bitbucket",
      "ghes.corp.internal": "GitHub Enterprise",
      "some.other.host": "Git host",
    });
  });

  it("buckets a git-pat-<slug> and an ssh-key-<slug> secret onto their known host", () => {
    const rows = deriveProviders(
      ["git-pat-dev-azure-com", "ssh-key-dev-azure-com"],
      ["dev.azure.com"],
      false,
    );
    expect(rows).toEqual([{ host: "dev.azure.com", brand: "Azure DevOps", lanes: ["pat", "ssh"] }]);
  });

  it("gives github.com an app lane FIRST when githubApp is true, creating the row if absent", () => {
    const rows = deriveProviders(["git-pat-github-com"], [], true);
    expect(rows).toEqual([
      // No scm_hosts entry, so the host string here came from the secret name.
      { host: "github.com", brand: "GitHub", lanes: ["app", "pat"], derivedFrom: "git-pat-github-com" },
    ]);
  });

  it("buckets a differently-cased scmHosts entry onto the same row", () => {
    // "GitHub.com" is storable: validSiteHost lowercases only to CHECK, so the
    // CLI/YAML/site-config PUT can persist it as typed. It must not open a
    // second row beside github.com's App lane.
    const rows = deriveProviders(["git-pat-github-com"], ["GitHub.com"], true);
    expect(rows).toEqual([{ host: "github.com", brand: "GitHub", lanes: ["app", "pat"] }]);
  });

  it("ignores names that aren't git-pat-*/ssh-key-* — github-app-* itself, arbitrary secrets, and legacy names", () => {
    const rows = deriveProviders(
      ["github-app-id", "github-app-key", "anthropic-api-key", ...LEGACY_NAMES],
      [],
      false,
    );
    expect(rows).toEqual([]);
  });

  // --- the bug this module must NOT port (wardyn-proto.js:204's naive
  // `slug.replace(/-/g, ".")` host recovery) ---

  it("(a) a host containing a real hyphen yields exactly ONE row, not two", () => {
    const rows = deriveProviders(["git-pat-git-server-corp-com"], ["git-server.corp.com"], false);
    expect(rows).toEqual([{ host: "git-server.corp.com", brand: "Git host", lanes: ["pat"] }]);
  });

  it("(b) an orphan secret still yields a usable best-guess row, FLAGGED as reconstructed", () => {
    const rows = deriveProviders(["git-pat-ghes-corp-internal"], [], false);
    expect(rows).toEqual([
      {
        host: "ghes.corp.internal",
        brand: "GitHub Enterprise",
        lanes: ["pat"],
        // The guess can be wrong (a real hyphen is indistinguishable from a
        // dot once slugged), so the row carries the name it was rebuilt from
        // and the UI keeps that invented hostname out of destructive copy.
        derivedFrom: "git-pat-ghes-corp-internal",
      },
    ]);
  });

  it("(d) a slug shared by two registered hosts credits BOTH — neither shows a false 'No credential'", () => {
    // slugHost is many-to-one: "git-server.corp.com" and "git.server.corp.com"
    // both slug to "git-server-corp-com". The secret's name cannot say which
    // one it is for, so picking the last-registered host was a false negative
    // on the other.
    const rows = deriveProviders(
      ["git-pat-git-server-corp-com"],
      ["git-server.corp.com", "git.server.corp.com"],
      false,
    );
    expect(rows).toEqual([
      { host: "git-server.corp.com", brand: "Git host", lanes: ["pat"] },
      { host: "git.server.corp.com", brand: "Git host", lanes: ["pat"] },
    ]);
  });

  it("(c) the registered hyphenated host wins over the naive dot-reversal guess — no phantom row appears", () => {
    const rows = deriveProviders(
      ["git-pat-git-server-corp-com"],
      ["git-server.corp.com", "unrelated.example.com"],
      false,
    );
    expect(rows).toEqual([
      { host: "git-server.corp.com", brand: "Git host", lanes: ["pat"] },
      { host: "unrelated.example.com", brand: "Git host", lanes: [] },
    ]);
    // The naive reverse of "git-server-corp-com" ("git.server.corp.com") must
    // not exist anywhere in the output — the credential landed on the
    // registered host, it didn't spawn a third, wrong one.
    expect(rows.find((r) => r.host === "git.server.corp.com")).toBeUndefined();
  });
});
