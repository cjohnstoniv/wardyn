/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { RunIntent } from "../../draft/run-intent";
import { mcpComponentIndex, setMcpHarnesses, type McpComponentRef } from "./harness-edits";

const mcp = (url: string): McpComponentRef => ({ name: "files", inline: { hosts: [], mcp: { url } } }) as unknown as McpComponentRef;
const intentOf = (components: McpComponentRef[]) => ({ components }) as unknown as RunIntent;

describe("an inline MCP row after the draft's components shift", () => {
  const first = mcp("https://one.example/mcp");
  const second = mcp("https://two.example/mcp");
  const answered = [first, second];

  it("finds the component the server answered, by position while the draft still holds it", () => {
    expect(mcpComponentIndex(intentOf(answered), "inline:1", "files", answered)).toBe(1);
  });

  it("stands for none when its component was removed, though another with the same name now sits at its place", () => {
    const afterRemoval = intentOf([second]);
    expect(mcpComponentIndex(afterRemoval, "inline:0", "files", answered)).toBe(-1);
    expect(setMcpHarnesses(afterRemoval, "inline:0", "files", ["codex-cli"], answered)).toBe(afterRemoval);
  });

  it("follows its component to its new place", () => {
    expect(mcpComponentIndex(intentOf([second]), "inline:1", "files", answered)).toBe(0);
  });

  it("still finds its component after the person toggled a harness on it, before the server answers again", () => {
    const toggled = setMcpHarnesses(intentOf(answered), "inline:1", "files", ["codex-cli"], answered);
    expect(mcpComponentIndex(toggled, "inline:1", "files", answered)).toBe(1);
    expect((toggled.components as McpComponentRef[])[1].mcp_harnesses).toEqual(["codex-cli"]);
    expect((toggled.components as McpComponentRef[])[0].mcp_harnesses).toBeUndefined();
  });
});
