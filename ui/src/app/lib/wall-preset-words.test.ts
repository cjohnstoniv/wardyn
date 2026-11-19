/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { WALL_PRESETS as W } from "../components/wardyn/copy/wall-presets";
import type { PersonalWallsSpec } from "./api/walls";
import { audienceOf, presetChips, presetRefusingEveryPush, sectionOfRefusal } from "./wall-preset-words";

const GH_READ = ["code_read", "issues_read", "identity"];
const ADO_READ = ["code_read", "work_read"];

describe("presetChips: every real limit has a chip, and Narrows nothing is only for an empty spec", () => {
  const cases: [string, PersonalWallsSpec, string[]][] = [
    ["empty", {}, [W.CHIP_OPEN]],
    ["deny_paths", { push_rules: { deny_paths: [".github/**", "infra/**"] } }, [W.CHIP_PUSH_PATHS(2)]],
    ["one deny path", { push_rules: { deny_paths: ["a/**"] } }, [W.CHIP_PUSH_PATHS(1)]],
    ["recording", { recording: "terminal" }, [W.CHIP_RECORDING]],
    ["auto_stop_after_sec", { auto_stop_after_sec: 900 }, [W.CHIP_AUTO_STOP]],
    ["allow_all_egress false", { allow_all_egress: false }, [W.CHIP_NO_ALLOW_ALL]],
    ["first use: ask for review", { first_use_approval: "deny_with_review" }, [W.CHIP_UNLISTED_REVIEW]],
    ["first use: hold", { first_use_approval: "wait_for_review" }, [W.CHIP_UNLISTED_HELD]],
    ["first use: refuse", { first_use_approval: "always_deny" }, [W.CHIP_UNLISTED_REFUSED]],
    ["tool rules", { tool_rules: [{ tool: "Bash", effect: "hold" }] }, [W.CHIP_TOOL_RULES(1)]],
    ["both forges read only", { github_capabilities: GH_READ, azure_devops_capabilities: ADO_READ }, [W.CHIP_READ_ONLY]],
    ["GitHub read only alone", { github_capabilities: GH_READ }, [W.CHIP_FORGE_READ("GitHub")]],
    ["Azure DevOps read only alone", { azure_devops_capabilities: ADO_READ }, [W.CHIP_FORGE_READ("Azure DevOps")]],
    ["GitHub list that still writes", { github_capabilities: ["code_read", "code_write"] }, [W.CHIP_FORGE_LIMITED("GitHub", 2)]],
    ["one forge read only, the other writes", { github_capabilities: GH_READ, azure_devops_capabilities: ["code_write"] }, [W.CHIP_FORGE_READ("GitHub"), W.CHIP_FORGE_LIMITED("Azure DevOps", 1)]],
    ["deny_all with a path", { push_rules: { deny_all: true, deny_paths: ["x/**"] } }, [W.CHIP_EVERY_PUSH, W.CHIP_PUSH_PATHS(1)]],
  ];
  it.each(cases)("%s", (_name, spec, want) => {
    expect(presetChips(spec)).toEqual(want);
    if (want[0] !== W.CHIP_OPEN) expect(presetChips(spec)).not.toContain(W.CHIP_OPEN);
  });

  it("never says an unqualified read-only for a spec that names one forge", () => {
    expect(presetChips({ github_capabilities: GH_READ })).not.toContain(W.CHIP_READ_ONLY);
  });
});

describe("audienceOf", () => {
  it.each([
    [{ scope: "builtin" as const }, W.AUD_BUILT_IN],
    [{ scope: "org" as const }, W.AUD_ORG],
    [{ scope: "person" as const }, W.AUD_YOURS],
    [{ scope: "group" as const, group_id: "g1" }, W.AUD_GROUP("Platform")],
  ])("%j", (p, want) => expect(audienceOf(p, { g1: "Platform" })).toBe(want));
});

describe("sectionOfRefusal", () => {
  it.each([
    ['invalid wall preset: denied_domains[1]: "x" must be a DNS name', "never"],
    ["invalid wall preset: allowed_domains: 300 entries exceeds the 256-entry limit", "sites"],
    ["invalid wall preset: first_use_approval: nope", "unlisted"],
    ["invalid wall preset: desktop.paste_in: walls only block", "clipboard"],
    ["invalid wall preset: withheld_grants[0].kind: unknown", "credentials"],
    ["invalid wall preset: push_rules.deny_all: walls only narrow", "pushes"],
    ["invalid wall preset: name is required", "name"],
    ["invalid wall preset: description must be printable text", "description"],
    ["invalid wall preset: github_capabilities[0]: nope", "github"],
    ["invalid wall preset: azure_devops_capabilities: name between 1 and 64 capabilities, or leave it out", "ado"],
    ["invalid wall preset: tool_rules[1]: duplicate rule for tool \"WebFetch\"", "tools"],
    ["invalid wall preset: push_rules.deny_paths: 99 entries", "pushes"],
    ["invalid wall preset: push_rules.deny_paths[2]: empty entry", "pushes"],
    ["invalid wall preset: recording: off is no floor", "recording"],
    ["invalid wall preset: auto_stop_after_sec: 0 is not a cap", "idle"],
    ["invalid walls: tool_rules[0]: tool is required", "tools"],
    ["invalid wall preset: the preset names no limit; Open is the built-in for no preset", null],
  ])("%s", (message, section) => expect(sectionOfRefusal(message).section).toBe(section));

  it("takes off either prefix and leaves the sentence as the server worded it", () => {
    expect(sectionOfRefusal("invalid walls: tool_rules[0]: tool is required").text).toBe("tool_rules[0]: tool is required");
    expect(sectionOfRefusal("invalid wall preset: recording: off is no floor").text).toBe("recording: off is no floor");
  });
});

describe("presetRefusingEveryPush", () => {
  it.each([
    ["the preset added deny_all", { name: "Docs triage", added: { push_rules: ["deny_all"] } }, "Docs triage"],
    ["the preset added other entries only", { name: "Docs triage", added: { denied_domains: ["x"] } }, undefined],
    ["the preset narrowed push_rules but added no deny_all", { name: "Docs triage", added: {} }, undefined],
    ["a personal preset a non-owner reads (no name)", { added: { push_rules: ["deny_all"] } }, undefined],
    ["no preset", undefined, undefined],
  ])("%s", (_n, p, want) => expect(presetRefusingEveryPush(p)).toBe(want));
});
