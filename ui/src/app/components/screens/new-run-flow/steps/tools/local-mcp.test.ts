/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { CLAUDE_UNGATED, CODEX, MCP_LOCAL, MCP_LOCAL_ENV, withFacts } from "../../../../../lib/new-run-fixtures/scenarios/tools-harness";
import { SCENARIO_ACCESS } from "../../../../../lib/new-run-fixtures/scenarios/tools";
import type { AccessRow } from "../../../../../lib/types/new-run-access";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import type { RunIntent } from "../../draft/run-intent";
import { buildBlockers } from "../../review/blockers";
import { mcpLocalRef } from "./catalogue-edits";
import { isLocalMcp, localMcpConflicts, localMcpRefused, sandboxSecrets, secretNames } from "./local-mcp";
import { commandProblem, defaultName, splitCommand } from "./mcp-local-form";

const LOCAL_ROW = MCP_LOCAL;
const ENV_CREDENTIAL = MCP_LOCAL_ENV;
const REMOTE_ROW: AccessRow = { ...LOCAL_ROW, id: "inline:1", name: "issue-tracker", mcp: { server_key: "issue-tracker", transport: "http", harnesses: ["codex-cli"] }, source: { kind: "component", id: "inline:1" } };
const DRAFT: RunIntent = { components: [mcpLocalRef("docs-search", "npx -y @example/mcp-server@1.4.2", ["docs.example.com"], { secret_name: "DOCS_TOKEN", delivery: { mode: "env", var: "DOCS_TOKEN" } })] };
const withRule = (tool: string, effect: "hold" | "deny" | "allow", id = "codex-cli"): RunIntent => ({ ...DRAFT, tools: [{ id, kind: "harness", tool_rules: [{ tool, effect }] }] });

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

// serverCases reads the ok and bad maps of TestComponentMCP_StdioLaunch: each `"name": localMCP("cmd", "arg", ...)`
// row whose arguments are plain string literals is a command line; any other row is skipped by name.
function serverCases(): { accepts: string[]; refuses: string[]; skipped: string[] } {
  const src = readFileSync(join(repoRoot(), "internal/types/component_mcp_test.go"), "utf8");
  const body = src.slice(src.indexOf("func TestComponentMCP_StdioLaunch("));
  const out = { accepts: [] as string[], refuses: [] as string[], skipped: [] as string[] };
  for (const [key, list] of [["ok", out.accepts], ["bad", out.refuses]] as const) {
    const from = body.indexOf(`${key} := map[string]types.ComponentDefinition{`);
    const block = body.slice(from, body.indexOf("\n\t}\n", from));
    for (const m of block.matchAll(/^\t\t"([^"]+)":\s*(.*),$/gm)) {
      const call = /^localMCP\(((?:"[^"\\]*"(?:, )?)+)\)$/.exec(m[2]);
      if (call) list.push([...call[1].matchAll(/"([^"]*)"/g)].map((a) => a[1]).join(" "));
      else out.skipped.push(m[1]);
    }
  }
  out.skipped.sort();
  return out;
}

describe("the command line", () => {
  it("splits a command and its arguments, with no shell", () => {
    expect(splitCommand("  npx  -y @example/mcp-server@1.4.2 ")).toEqual({ command: "npx", args: ["-y", "@example/mcp-server@1.4.2"] });
    expect(splitCommand("")).toEqual({ command: "", args: [] });
  });

  it.each([
    ["", "empty"],
    ["npx -y @example/mcp-server", "unpinned"],
    ["npx -y @example/mcp-server@latest", "unpinned"],
    ["npx -y @example/mcp-server@^1.4.2", "unpinned"],
    ["npx", "unpinned"],
    ["npx -y @example/mcp-server@1.4.2", undefined],
    ["npx mcp-server@1.4.2-rc.1", undefined],
    ["/usr/local/bin/docs-mcp --stdio", undefined],
  ])("%j is %s", (text, problem) => {
    expect(commandProblem(text)).toBe(problem);
  });

  it("ends the server's two sentences with a period, as the overseer ruled", () => {
    expect(NR_TOOLS.MCP_ERR_PROGRAM("uvx")).toBe('"uvx" must be npx or the absolute path of a program in the image.');
    expect(NR_TOOLS.MCP_ERR_NPX_FLAG).toBe("npx takes only -y before the package here; name the package as its first argument.");
  });

  // The server's own cases, read by path from TestComponentMCP_StdioLaunch (internal/types/component_mcp_test.go), as
  // wire-parity.test.ts reads Go source. Not mirrored: a url, a missing launch, a control byte inside one argument and
  // the argument-count cap, which the command line field cannot express or has no sentence for.
  const NOT_MIRRORED = ["control byte", "no launch", "too many args", "url"];
  const server = serverCases();

  it("reads every server case it does not skip by name", () => {
    expect(server.skipped).toEqual(NOT_MIRRORED);
    expect(server.accepts.length).toBeGreaterThanOrEqual(4);
    expect(server.refuses.length).toBeGreaterThanOrEqual(9);
  });

  it.each(server.accepts)("accepts what the server accepts: %s", (text) => {
    expect(commandProblem(text)).toBeUndefined();
  });

  it.each(server.refuses)("refuses what the server refuses: %s", (text) => {
    expect(commandProblem(text)).toBeDefined();
  });

  // Not in the server's table, kept from the client's own cases.
  it.each([
    ["uvx foo", "program"],
    ["/opt/mcp/", "program"],
    ["npx --package x", "npx-flag"],
  ] as const)("refuses %s as %s", (text, problem) => {
    expect(commandProblem(text)).toBe(problem);
  });

  it("names an unnamed server for its package or program", () => {
    expect(defaultName("npx -y @example/mcp-server@1.4.2")).toBe("mcp-server");
    expect(defaultName("/opt/tools/docs-mcp --stdio")).toBe("docs-mcp");
  });

  it("builds the component the server reads: stdio and a launch, no url", () => {
    expect(mcpLocalRef("docs-search", "npx -y @example/mcp-server@1.4.2", ["docs.example.com"])).toEqual({
      name: "docs-search",
      inline: { hosts: ["docs.example.com"], mcp: { transport: "stdio", launch: { command: "npx", args: ["-y", "@example/mcp-server@1.4.2"] } } },
    });
  });
});

describe("a server that runs in the sandbox", () => {
  const facts = (...rows: AccessRow[]) => withFacts(SCENARIO_ACCESS.member, [CLAUDE_UNGATED, CODEX], rows);

  it("is an MCP row on the stdio transport", () => {
    expect(isLocalMcp(LOCAL_ROW)).toBe(true);
    expect(isLocalMcp(REMOTE_ROW)).toBe(false);
  });

  it.each([
    ["a hold on one of its tools", withRule("mcp__docs-search__write_page", "hold"), true],
    ["a block on one of its tools", withRule("mcp__docs-search__write_page", "deny"), true],
    ["a block on every tool", withRule("*", "deny"), true],
    ["an allow, which is what happens anyway", withRule("mcp__docs-search__write_page", "allow"), false],
    ["a rule for another server's tool", withRule("mcp__issue-tracker__write_page", "deny"), false],
    ["a rule for a built-in tool", withRule("WebFetch", "deny"), false],
    ["no rule", DRAFT, false],
  ])("is refused for Codex CLI under %s: %s", (_name, intent, refused) => {
    expect(localMcpRefused(CODEX, intent, LOCAL_ROW)).toBe(refused);
  });

  it("counts the person's default and the policy's locked rules", () => {
    expect(localMcpRefused(CODEX, { ...DRAFT, tools: [{ id: "codex-cli", kind: "harness", default_effect: "hold" }] }, LOCAL_ROW)).toBe(true);
    const locked = { ...CODEX, policy_rules: [{ tool: "mcp__docs-search__x", effect: "deny", source: { kind: "policy" as const } }] };
    expect(localMcpRefused(locked, DRAFT, LOCAL_ROW)).toBe(true);
  });

  it("is never refused for Claude Code, which applies the rules itself, nor when it is a remote server", () => {
    expect(localMcpRefused(CLAUDE_UNGATED, withRule("mcp__docs-search__x", "deny", "claude-code"), LOCAL_ROW)).toBe(false);
    expect(localMcpRefused(CODEX, withRule("mcp__issue-tracker__x", "deny"), REMOTE_ROW)).toBe(false);
  });

  it("lists each server turned on for a harness that cannot apply its rules", () => {
    const intent = withRule("mcp__docs-search__write_page", "deny");
    expect(localMcpConflicts(facts(LOCAL_ROW, REMOTE_ROW), intent)).toEqual([{ harness: "codex-cli", server: "docs-search" }]);
    expect(localMcpConflicts(facts(LOCAL_ROW), DRAFT)).toEqual([]);
    expect(localMcpConflicts(facts({ ...LOCAL_ROW, mcp: { ...LOCAL_ROW.mcp!, harnesses: ["claude-code"] } }), intent)).toEqual([]);
  });

  it("names the secret from the draft's own component, else says the credential's label", () => {
    const access = { ...facts(LOCAL_ROW), credentials: [ENV_CREDENTIAL] };
    expect(secretNames(access, DRAFT).get(ENV_CREDENTIAL.id)).toBe("DOCS_TOKEN");
    expect(sandboxSecrets(access, DRAFT)).toEqual([{ server: "docs-search", secret: "DOCS_TOKEN" }]);
    expect(sandboxSecrets(access, {})).toEqual([{ server: "docs-search", secret: "Your secret" }]);
    expect(sandboxSecrets({ ...access, credentials: [{ ...ENV_CREDENTIAL, delivery: "proxy" }] }, DRAFT)).toEqual([]);
    expect(sandboxSecrets({ ...access, credentials: [{ ...ENV_CREDENTIAL, label: undefined }] }, {})).toEqual([{ server: "docs-search", secret: undefined }]);
  });
});

describe("Review blocker for a local server", () => {
  const input = { issues: [], unsupported: [], templateNeeds: [], setupItems: [], stale: false };
  const localMcp = [{ harness: "codex-cli", server: "docs-search" }];

  it("holds Launch with the board's sentence, linked to Tools and access", () => {
    expect(buildBlockers({ ...input, refusal: null, localMcp, harnessLabel: () => "Codex CLI" })).toEqual([
      { id: "local_mcp:codex-cli:docs-search", kind: "refusal", text: NR_TOOLS.MCP_REFUSED("Codex CLI", "docs-search"), to: { screen: "step", step: "tools" } },
    ]);
  });

  it("is said once: the server's own refusal of the same run stands down, any other refusal stays", () => {
    const own = buildBlockers({ ...input, localMcp, refusal: { text: "In this run Codex CLI can't apply the rule", reason: "request_field_unavailable" } });
    expect(own.map((b) => b.id)).toEqual(["local_mcp:codex-cli:docs-search"]);
    const other = buildBlockers({ ...input, localMcp, refusal: { text: "Offline.", reason: "runner_offline" } });
    expect(other.map((b) => b.id)).toEqual(["local_mcp:codex-cli:docs-search", "refusal"]);
  });
});
