/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import {
  buildOverrides,
  buildRunContractWire,
  emptyRunContractDraft,
  inactiveOverrides,
  NO_ACTIVE_SECTIONS,
  overrideItems,
  pushRuleKey,
  resourcesWire,
  type ActiveSections,
  type OverrideDraft,
} from "./run-contract-draft";

const everything: ActiveSections = { agent: true, azureDevOps: true, gitPATHosts: ["git.example.com"], pushKeys: ["github/acme", "azure_devops/globex"] };

const draft: OverrideDraft = {
  agent: { add_hosts: ["api.example.com"], tool_rules: [{ tool: "Bash", effect: "hold" }] },
  azureDevOps: { capabilities: ["code_read"] },
  gitPAT: [{ host: "git.example.com", access: "read" }, { host: "other.example.com", access: "read" }],
  pushRules: [
    { provider: "github", org: "acme", deny_paths: ["infra/"] },
    { provider: "github", org: "other", deny_paths: ["ci/"] },
  ],
};

describe("run contract draft", () => {
  it("sends the overrides of active sections only, and keeps the rest in the draft", () => {
    expect(buildOverrides(draft, NO_ACTIVE_SECTIONS)).toBeUndefined();
    expect(buildOverrides(draft, everything)).toEqual({
      agent: draft.agent,
      azure_devops: draft.azureDevOps,
      git_pat: [draft.gitPAT[0]],
      push_rules: [draft.pushRules[0]],
    });
    const some: ActiveSections = { ...NO_ACTIVE_SECTIONS, pushKeys: ["github/other"] };
    expect(buildOverrides(draft, some)).toEqual({ push_rules: [draft.pushRules[1]] });
    // The unsent edits are still there, so re-adding the section brings them back.
    const kept = inactiveOverrides(draft, some);
    expect(kept.agent).toEqual(draft.agent);
    expect(kept.azureDevOps).toEqual(draft.azureDevOps);
    expect(kept.gitPAT).toHaveLength(2);
    expect(kept.pushRules).toEqual([draft.pushRules[0]]);
    expect(draft.pushRules).toHaveLength(2);
  });

  it("files push-rule overrides by provider and organisation", () => {
    expect(pushRuleKey({ provider: "github", org: "acme" })).toBe("github/acme");
    expect(pushRuleKey({ provider: "azure_devops", org: "acme" })).not.toBe(pushRuleKey({ provider: "github", org: "acme" }));
    // One section's rules never ride with another's.
    expect(buildOverrides(draft, { ...NO_ACTIVE_SECTIONS, pushKeys: ["azure_devops/acme"] })).toBeUndefined();
  });

  it("sends nothing for an empty edit", () => {
    expect(buildOverrides({ agent: { add_hosts: [] }, azureDevOps: { capabilities: [] }, gitPAT: [], pushRules: [] }, everything)).toBeUndefined();
  });

  it("sends only what the person chose", () => {
    expect(buildRunContractWire(undefined, everything)).toEqual({});
    expect(buildRunContractWire(emptyRunContractDraft(), everything)).toEqual({});
    expect(buildRunContractWire({ ...emptyRunContractDraft(), placement: "remote", runnerId: "r1", cpus: 4, memoryMiB: 8192 }, NO_ACTIVE_SECTIONS)).toEqual({
      placement: "remote",
      resources: { cpu_millis: 4000, memory_mib: 8192 },
    });
    expect(buildRunContractWire({ ...emptyRunContractDraft(), placement: "local", runnerId: "r1" }, NO_ACTIVE_SECTIONS)).toEqual({
      placement: "local",
      runner_id: "r1",
    });
  });

  it("converts tenths of a CPU and whole MiB, and leaves an invalid field out", () => {
    expect(resourcesWire(0.5, 512)).toEqual({ cpu_millis: 500, memory_mib: 512 });
    expect(resourcesWire(1.3)).toEqual({ cpu_millis: 1300 });
    expect(resourcesWire(0, 1.5)).toBeUndefined();
    expect(resourcesWire(Number.NaN, -1)).toBeUndefined();
  });

  it("lists edits in the narrowing table's terms, as the server does", () => {
    expect(
      overrideItems({
        agent: {
          add_hosts: ["a.example.com", "*.example.com"],
          remove_hosts: ["b.example.com"],
          add_secrets: [{ secret_name: "k", host: "a.example.com" }],
          remove_secrets: ["j"],
          tool_rules: [{ tool: "Bash", effect: "deny" }, { tool: "Read", effect: "allow" }],
        },
        azure_devops: { capabilities: ["code_read"] },
        git_pat: [{ host: "g", access: "read" }, { host: "h", access: "write" }, { host: "i", repos: [] }],
        push_rules: [{ provider: "github", org: "acme", deny_paths: ["a/"], require_review_paths: ["b/"] }],
      }).map((i) => `${i.kind} ${i.op}`),
    ).toEqual([
      "agent_host add",
      "agent_host_wildcard add",
      "agent_host remove",
      "agent_secret add",
      "agent_secret remove",
      "tool_rule_restrict add",
      "tool_rule_allow add",
      "ado_capability narrow",
      "git_pat_scope narrow",
      "git_pat_scope add",
      "git_pat_scope narrow",
      "push_rule_deny add",
      "push_rule_review add",
    ]);
  });
});
