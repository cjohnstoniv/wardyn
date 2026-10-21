/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The review card keeps its own copy of the hosts the server never promotes from a recording. These
// pin its static lists to the Go source, so a host added on either side fails here, not as a 422 on Approve.
import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { BROKER_HOSTS, HARNESS_HOSTS } from "./session-helpers";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

const goSource = (file: string): string => readFileSync(join(repoRoot(), "internal/api", file), "utf8");
const quoted = (s: string): string[] => [...s.matchAll(/"([^"]+)"/g)].map((m) => m[1]);

describe("record review's plumbing-host copy matches record.go's skip set", () => {
  it("BROKER_HOSTS is gitBrokerManagedHosts plus the brokered forges' SSH hosts", () => {
    const managed = /var gitBrokerManagedHosts = \[\]string\{([^}]*)\}/.exec(goSource("workspace_run.go"));
    expect(managed).not.toBeNull();
    const forges = /var gitBrokerForges = \[\]string\{([^}]*)\}/.exec(goSource("runs_dispatch_gitbroker.go"));
    expect(forges).not.toBeNull();
    const sshOf = new Map<string, string>();
    for (const m of goSource("runs_scm.go").matchAll(/case ([^:\n]+):\s*\n\s*return "([^"]+):443", true/g)) {
      for (const name of quoted(m[1])) sshOf.set(name, m[2]);
    }
    const ssh = quoted(forges![1]).flatMap((f) => (sshOf.has(f) ? [sshOf.get(f)!] : []));
    expect(ssh.length).toBeGreaterThan(0);
    expect([...BROKER_HOSTS].sort()).toEqual([...quoted(managed![1]), ...ssh].sort());
  });

  it("HARNESS_HOSTS is isModelProviderHost's vendor suffixes (bare and wildcard) and exact hosts", () => {
    const body = /func \(s \*Server\) isModelProviderHost\(h string\) bool \{([\s\S]*?)\n\}/.exec(goSource("llmcred.go"));
    expect(body).not.toBeNull();
    const suffixes = [...body![1].matchAll(/HasSuffix\(hl, "([^"]+)"\)/g)].map((m) => m[1]);
    const exact = [...body![1].matchAll(/hl == "([^"]+)"/g)].map((m) => m[1]);
    expect(suffixes.length + exact.length).toBeGreaterThan(0);
    expect([...HARNESS_HOSTS].sort()).toEqual([...suffixes, ...suffixes.map((s) => `*.${s}`), ...exact].sort());
  });
});
