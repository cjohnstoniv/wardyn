/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F8-ui-types-mirror probe: pins runWireBody's wire-forwarding to the Go DTO
// (sibling of ui/src/app/lib/api/runs.wire.test.ts; imports `./runs` and
// `./core` relative to this directory).
//
// Run (from the repo root, no PG needed — fetch is stubbed):
//   cd ui && pnpm vitest run src/app/lib/api/runs.wire.fields.test.ts
//
// What it pins, in three layers:
//   A. runWireBody (runs.ts:70-116) forwards every field of the Go DTO
//      pkg/client.CreateRunRequest (runs_create.go) that the console can
//      set, on both createRun and preflightRun, and omits the key when the
//      input has no value for it. runs.wire.test.ts:42-64 covers only
//      workspaces[] and integration_id; the eleven other forwarded fields
//      have no assertion anywhere else.
//   B. The whitelist's deliberate drops are pinned too (devcontainer_repo /
//      devcontainer_ref are CLI-only and not forwarded), so a change in
//      either direction is a visible test change, never a silent one.
//   C. Source parity, from the TS side — the counterpart of Go's
//      TestTerminalRunStates_UIParity (internal/types/terminal_parity_test.go):
//      every JSON tag on the Go CreateRunRequest is either forwarded by
//      runWireBody or on the explicit UI-never-sends list; every key the TS
//      AgentRun interface (types/runs.ts:57-122) declares is a JSON tag on Go's
//      types.AgentRun (types.go:129-212) or on handleGetRun's wrapper
//      (runs_policy.go:172-175). A Go rename (e.g. failure_hint -> failure_reason)
//      fails here instead of as a runtime `undefined` the console renders as "—".
//      The Go side is read from source with the same regex discipline the Go
//      parity test uses; the repo root is found by walking up to go.mod so the
//      file works from ui/ (vitest's cwd) or anywhere under it.

import { previewRunPolicy } from "./policy-preview";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { runs, runWireBody } from "./runs";
import type { RunPolicySpec } from "../types";

// The body type createRun/preflightRun accept (RunWireInput is not exported).
type WireInput = Parameters<typeof runs.createRun>[0];

// A minimal VALID RunPolicySpec (its three required keys) with the floor set.
const policyWithFloor = (floor: string): RunPolicySpec =>
  ({ min_confinement_class: floor, allowed_domains: ["api.anthropic.com"], first_use_approval: "always_deny" }) as unknown as RunPolicySpec;

// Shared fetch stub — same shape as runs.wire.test.ts:19-30.
let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(() => {
  // A FRESH Response per call: a Response body can only be read once, and the
  // byte-identical-bodies case below calls fetch twice inside one test
  // (mockResolvedValue would hand it the same, already-consumed instance).
  fetchMock = vi.fn().mockImplementation(async () =>
    new Response(JSON.stringify({ id: "run_1" }), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => vi.unstubAllGlobals());

const sentBody = (): Record<string, unknown> =>
  JSON.parse(String(fetchMock.mock.calls[0][1]?.body ?? "{}"));
const sentPath = (): string => String(fetchMock.mock.calls[0][0]);

// Every field the console can put on the wire, with a NON-default value.
// One entry per Go DTO field the UI forwards (CreateRunRequest ∩ runWireBody).
const fullInput: WireInput & { workspaces: NonNullable<WireInput["workspaces"]> } = {
  agent: "claude-code",
  repo: "acme/payments",
  task: "do the thing",
  title: "Refund flow",
  description: "ticket 4412",
  policy_id: "11111111-1111-1111-1111-111111111111",
  confinement_class: "CC2" as const,
  interactive: true,
  inline_policy: policyWithFloor("CC1"),
  image: "ghcr.io/acme/dev:1",
  task_mode: "exec" as const,
  interactive_start: "agent" as const,
  seed_auto_tools: true,
  tool_approvals: "hold" as const,
  workspaces: [{ workspace_id: "ws-1", enabled_optional: ["egress:api.stripe.com"], read_only: true, target: "/home/agent/work" }],
  workspace_id: "22222222-2222-2222-2222-222222222222",
  integration_id: "anthropic_api_key",
  // The member's drive request (D5): forwarded verbatim by runWireBody — a
  // path never rides here, only {enabled, read_only}.
  drive: { enabled: true, read_only: false },
  // The components this run carries (#1914): a stored one by id, a run-only inline one, a built-in Git provider.
  components: [
    { id: "33333333-3333-4333-8333-333333333333" },
    { inline: { hosts: ["api.example"] }, name: "Tool" },
    { builtin: "github", org: "acme", repos: ["acme/api"], access: "read" },
  ],
  // The 0.9 New Run contract: where the run lives, its size, the person's edits.
  placement: "local",
  runner_id: "44444444-4444-4444-8444-444444444444",
  runner_pool_id: "55555555-5555-4555-8555-555555555555",
  allowed_image: "ghcr.io/acme/dev:1",
  resources: { cpu_millis: 4000, memory_mib: 8192 },
  overrides: { azure_devops: { capabilities: ["code_read"] } },
};

// The wire keys runWireBody is expected to emit for fullInput, and the exact
// value each carries. Note the clamp: confinement_class CC2 vs floor CC1 is
// already >= floor, so it passes through unchanged (runs.ts:82-85).
const expectedWire: Record<string, unknown> = {
  agent: "claude-code",
  repo: "acme/payments",
  task: "do the thing",
  title: "Refund flow",
  description: "ticket 4412",
  policy_id: "11111111-1111-1111-1111-111111111111",
  confinement_class: "CC2",
  interactive: true,
  inline_policy: fullInput.inline_policy,
  image: "ghcr.io/acme/dev:1",
  task_mode: "exec",
  interactive_start: "agent",
  seed_auto_tools: true,
  tool_approvals: "hold",
  workspaces: fullInput.workspaces,
  workspace_id: "22222222-2222-2222-2222-222222222222",
  integration_id: "anthropic_api_key",
  // The member's drive request (D5): forwarded verbatim by runWireBody — a
  // path never rides here, only {enabled, read_only}.
  drive: { enabled: true, read_only: false },
  components: fullInput.components,
  placement: "local",
  runner_id: "44444444-4444-4444-8444-444444444444",
  runner_pool_id: "55555555-5555-4555-8555-555555555555",
  allowed_image: "ghcr.io/acme/dev:1",
  resources: { cpu_millis: 4000, memory_mib: 8192 },
  overrides: { azure_devops: { capabilities: ["code_read"] } },
};

// A new client's request: the run-mode carriers replace the older mode fields,
// so this body is the second half of "every CreateRunRequest tag is forwarded".
const fullModeInput: WireInput = {
  repo: "acme/payments",
  experience: "interactive",
  tools: [{ id: "claude-code", kind: "harness", model_provider: "bedrock-team" }],
  startup: { kind: "harness", tool: "claude-code" },
  start_folder: { kind: "attachment", attachment: "drive", subpath: "src" },
  workload: { kind: "agent_task", agent: "claude-code", task: "t" },
  no_repositories_or_drives: true,
  // What the older mode fields would say: a new client's request must not carry them.
  agent: "claude-code",
  task: "legacy",
  interactive: true,
  task_mode: "exec",
  interactive_start: "agent",
  seed_auto_tools: true,
  tool_approvals: "hold",
  model_provider: "bedrock-team",
};

// The older mode fields the carriers replace (Go: runModeLegacyField).
const REPLACED_BY_RUN_MODE = ["agent", "task", "interactive", "task_mode", "interactive_start", "seed_auto_tools", "tool_approvals", "model_provider"];

// Go DTO JSON tags the console NEVER sends (CLI-only — cmd/wardyn/commands.go:96-103).
// If the whitelist starts forwarding one of these, or the Go DTO drops one,
// this list must change in the same commit. model_provider is CLI/API-only
// until the New Run rail's provider picker lands (multi-provider MP-23), which
// moves it into runWireBody. preset/preset_version are the launcher API's
// (#1143); the console sends the explicit spec and has no preset UI.
const UI_NEVER_SENDS = new Set([
  "devcontainer_repo",
  "devcontainer_ref",
  "model_provider",
  "preset",
  "preset_version",
]);

// ui_apps used to sit on this set as a TS AgentRun key with no Go AgentRun
// json tag (handleGetRun's anonymous wrapper struct, runs_policy.go:172-175,
// GET /runs/{id} only) — it now lives on RunDetail instead (see the RunDetail
// test below), so AgentRun itself needs no wrapper-key exclusion.

// A. Every forwarded field reaches both doors with the value the caller set.
describe("runWireBody — every console-settable DTO field reaches the wire", () => {
  // ticket: F8
  for (const door of ["createRun", "preflightRun"] as const) {
    it(`${door}: forwards all ${Object.keys(expectedWire).length} fields verbatim`, async () => {
      await runs[door](fullInput);
      const body = sentBody();
      // Exact key set — an extra key would be a strict-decode 400 on the Go
      // side (helpers.go:391-398); a missing key is the silent-drop class.
      expect(Object.keys(body).sort()).toEqual(Object.keys(expectedWire).sort());
      for (const [k, v] of Object.entries(expectedWire)) {
        expect(body[k], `field ${k} on ${door}`).toEqual(v);
      }
      expect(sentPath()).toBe(door === "createRun" ? "/api/v1/runs" : "/api/v1/runs/preflight");
    });
  }

  it("create, preflight and preview send byte-identical bodies for the same input", async () => {
    await runs.createRun(fullInput);
    const a = String(fetchMock.mock.calls[0][1]?.body);
    fetchMock.mockClear();
    await runs.preflightRun(fullInput);
    const b = String(fetchMock.mock.calls[0][1]?.body);
    await previewRunPolicy(fullInput);
    expect(a).toBe(b);
    expect(String(fetchMock.mock.calls[1][1]?.body)).toBe(a);
    expect(String(fetchMock.mock.calls[1][0])).toMatch(/\/runs\/policy-preview$/);
  });

  it("calls the registered policy-preview route", async () => {
    const routes = readFileSync(join(repoRoot(), "internal/api/routes.go"), "utf8");
    const route = /r\.Post\("([^"]+)", s\.handlePolicyPreview\)/.exec(routes)?.[1];
    expect(route).toBe("/runs/policy-preview");
    fetchMock.mockImplementation(async (url: RequestInfo | URL) => new Response(JSON.stringify({ spec: fullInput.inline_policy }), {
      status: String(url) === `/api/v1${route}` ? 200 : 404,
      headers: { "content-type": "application/json" },
    }));
    await expect(previewRunPolicy(fullInput)).resolves.toMatchObject({ spec: fullInput.inline_policy });
    expect(fetchMock.mock.calls[0][1]?.method).toBe("POST");
  });

  it("preserves Retry-After on a preview refusal for the bounded preview scheduler", async () => {
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ error: "limited" }), { status: 429, headers: { "Retry-After": "3" } }));
    await expect(previewRunPolicy(fullInput)).rejects.toMatchObject({ status: 429, message: "limited", retryAfter: "3" });
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ error: "limited" }), { status: 429 }));
    await expect(previewRunPolicy(fullInput)).rejects.toMatchObject({ status: 429, retryAfter: undefined });
  });

  it("omits components when the run carries none, however it is spelled", async () => {
    for (const none of [undefined, []]) {
      fetchMock.mockClear();
      await runs.createRun({ ...fullInput, components: none });
      expect(sentBody()).not.toHaveProperty("components");
    }
  });

  // Per-field omission: an ABSENT choice must be an absent key, never
  // "", false, or null — pkg/client/dto_parity_test.go pins the same rule
  // from the Go side ("a minimal request must put only the always-sent keys
  // on the wire").
  const optionalKeys = Object.keys(expectedWire).filter((k) => !["agent", "repo", "task"].includes(k));
  for (const k of optionalKeys) {
    it(`omits ${k} when the input does not set it`, async () => {
      const input: Record<string, unknown> = { ...fullInput };
      delete input[k];
      // policy_id and inline_policy are XOR on the server; dropping one is legal.
      await runs.createRun(input as unknown as WireInput);
      expect(k in sentBody(), `${k} must be absent, not empty`).toBe(false);
    });
  }

  it("falsy scalars are omitted (interactive:false, seed_auto_tools:false, empty strings)", async () => {
    const falsy: WireInput = {
      agent: "claude-code",
      repo: "",
      task: "",
      title: "",
      description: "",
      interactive: false,
      seed_auto_tools: false,
      image: "",
      task_mode: undefined,
      integration_id: "",
      workspaces: [],
    };
    await runs.createRun(falsy);
    const body = sentBody();
    // repo/task are ALWAYS-sent keys (runs.ts:71-75) — JSON.stringify keeps "".
    expect(Object.keys(body).sort()).toEqual(["agent", "repo", "task"]);
  });

  it("workspaces[].read_only:false is forwarded as false (Go *bool no-op), not dropped", async () => {
    await runs.createRun({
      agent: "claude-code",
      repo: "r",
      task: "t",
      workspaces: [{ workspace_id: "ws-1", read_only: false }],
    });
    expect(sentBody().workspaces).toEqual([{ workspace_id: "ws-1", read_only: false }]);
  });
});

// B. The confinement clamp (runs.ts:82-85 + core.ts:66) — pinned exactly,
//    including the two edge cases below (H3a, H3b).
describe("runWireBody — confinement_class clamp against inline_policy.min_confinement_class", () => {
  const base: WireInput = { agent: "claude-code", repo: "r", task: "t" };

  it("raises a requested class UP to the inline floor", async () => {
    await runs.createRun({ ...base, confinement_class: "CC1", inline_policy: policyWithFloor("CC3") });
    expect(sentBody().confinement_class).toBe("CC3");
  });

  it("leaves a class at or above the floor alone", async () => {
    await runs.createRun({ ...base, confinement_class: "CC3", inline_policy: policyWithFloor("CC2") });
    expect(sentBody().confinement_class).toBe("CC3");
  });

  it("does not clamp against a policy_id run (no inline floor is visible client-side)", async () => {
    await runs.createRun({ ...base, confinement_class: "CC1", policy_id: "11111111-1111-1111-1111-111111111111" });
    expect(sentBody().confinement_class).toBe("CC1");
  });

  // H3: an unrecognised class ranks 0 (core.ts:66), so it is silently
  // replaced by the floor instead of reaching the server's 400
  // (runs.go:40-49). This pins today's behaviour so a fix (or a regression)
  // is visible; the assertion is on what the code does, not on what is ideal.
  it("an unrecognised requested class is replaced by the floor (server 400 is masked)", async () => {
    // ticket: H3a
    await runs.createRun({ ...base, confinement_class: "cc2" as never, inline_policy: policyWithFloor("CC2") });
    expect(sentBody().confinement_class).toBe("CC2");
  });

  it("an unrecognised FLOOR disables the clamp entirely", async () => {
    // ticket: H3b
    await runs.createRun({ ...base, confinement_class: "CC1", inline_policy: policyWithFloor("vault") });
    expect(sentBody().confinement_class).toBe("CC1");
  });
});

// C. Source parity with the Go wire — the TS-side twin of
//    internal/types/terminal_parity_test.go.
function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

// Extract the json tag names of one Go struct body: `type <name> struct { ... }`.
// Nested braces (none in these structs) are not handled — the Go parity test
// has the same discipline. `json:"-"` and untagged fields are skipped.
function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  const tags: string[] = [];
  for (const t of m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)) {
    if (t[1] !== "-") tags.push(t[1]);
  }
  return tags;
}

// Extract the property names of one TS interface body.
function tsInterfaceKeys(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`interface ${name} not found`);
  const keys: string[] = [];
  for (const line of m[1].split("\n")) {
    // R-04: `(?:readonly\s+)?` — the regex must match an optional `readonly`
    // modifier, or a mirrored `readonly foo?:` field silently reads as absent
    // from the TS side (see the SiteConfig test below).
    const k = /^\s*(?:readonly\s+)?([A-Za-z_][A-Za-z0-9_]*)\??\s*:/.exec(line);
    if (k) keys.push(k[1]);
  }
  return keys;
}

describe("source parity — Go wire tags vs the TS mirror", () => {
  // ticket: F8
  const root = repoRoot();
  // CreateRunRequest and WorkspaceSelection live in runs_create.go; read the
  // package's two DTO files together so a later move within pkg/client is not a false red.
  const clientGo = ["client.go", "runs_create.go", "runs_new_run.go", "runs_mode.go"].map((f) => readFileSync(join(root, "pkg/client", f), "utf8")).join("\n");
  const typesGo = readFileSync(join(root, "internal/types/types.go"), "utf8");
  const runsTs = readFileSync(join(root, "ui/src/app/lib/types/runs.ts"), "utf8");
  const runCreateTs = readFileSync(join(root, "ui/src/app/lib/types/run-create.ts"), "utf8");

  it("every CreateRunRequest json tag is forwarded by runWireBody or on the UI-never-sends list", async () => {
    const goTags = goJSONTags(clientGo, "CreateRunRequest");
    expect(goTags.length).toBeGreaterThanOrEqual(17); // stale-regex guard (Go test does the same)
    const wireKeys = new Set([...Object.keys(runWireBody(fullInput)), ...Object.keys(runWireBody(fullModeInput))]);
    const missing = goTags.filter((t) => !wireKeys.has(t) && !UI_NEVER_SENDS.has(t));
    expect(
      missing,
      "Go DTO fields the console can never send — add them to runWireBody (runs.ts:70-116) " +
        "and fullInput above, or to UI_NEVER_SENDS with a reason",
    ).toEqual([]);
    // …and the reverse: nothing on UI_NEVER_SENDS has quietly been removed from Go.
    for (const t of UI_NEVER_SENDS) {
      expect(goTags, `UI_NEVER_SENDS names ${t}, which is no longer a Go DTO field`).toContain(t);
    }
  });

  it("every key runWireBody emits is a CreateRunRequest json tag (a stray key is a strict-decode 400)", async () => {
    const goTags = new Set(goJSONTags(clientGo, "CreateRunRequest"));
    const stray = [...Object.keys(runWireBody(fullInput)), ...Object.keys(runWireBody(fullModeInput))].filter((k) => !goTags.has(k));
    expect(stray, "keys the server will reject with 400 'unknown field' (helpers.go:393)").toEqual([]);
  });

  it("a new client's request carries the run-mode carriers and none of the older mode fields", async () => {
    for (const door of ["createRun", "preflightRun"] as const) {
      fetchMock.mockClear();
      await runs[door](fullModeInput);
      const body = sentBody();
      for (const key of ["experience", "tools", "startup", "start_folder", "workload", "no_repositories_or_drives", "repo"]) {
        expect(body, `${key} on ${door}`).toHaveProperty(key);
      }
      for (const key of REPLACED_BY_RUN_MODE) expect(body, `${key} on ${door}`).not.toHaveProperty(key);
    }
  });

  it("an older client's request carries none of the run-mode carriers", () => {
    const body = runWireBody(fullInput);
    for (const key of ["experience", "tools", "startup", "start_folder", "workload", "no_repositories_or_drives"]) {
      expect(body).not.toHaveProperty(key);
    }
    for (const key of REPLACED_BY_RUN_MODE.filter((k) => k !== "model_provider")) expect(body).toHaveProperty(key);
  });

  it("WorkspaceSelection: the TS wire entry's keys are exactly Go's json tags", () => {
    const goTags = goJSONTags(clientGo, "WorkspaceSelection").sort();
    const sent = Object.keys(fullInput.workspaces[0]).sort();
    expect(sent).toEqual(goTags);
  });

  it("every TS AgentRun key is a Go types.AgentRun json tag", () => {
    const goTags = new Set(goJSONTags(typesGo, "AgentRun"));
    expect(goTags.size).toBeGreaterThanOrEqual(20);
    const tsKeys = tsInterfaceKeys(runsTs, "AgentRun");
    expect(tsKeys.length).toBeGreaterThanOrEqual(20);
    const unknown = tsKeys.filter((k) => !goTags.has(k));
    expect(
      unknown,
      "TS reads these off the run payload but Go never writes them — a rename on the Go side " +
        "(the runtime-undefined class the mirror comment at runs.ts:124-128 warns about)",
    ).toEqual([]);
  });

  it("every Go AgentRun tag is mirrored on the TS interface (agent_exec_id/auto_stop_after_sec/source_id included)", () => {
    // ticket: F8
    const goTags = goJSONTags(typesGo, "AgentRun");
    const tsKeys = new Set(tsInterfaceKeys(runsTs, "AgentRun"));
    const omitted = goTags.filter((t) => !tsKeys.has(t));
    expect(omitted).toEqual([]);
  });

  it("RunDetail adds exactly ui_apps, user_type_name, the model provider's name and deleted flag, the launching portal's name, the ended run's kept_until, the run's policy and its open sync sessions over AgentRun — " +
    "the fields only GET /runs/{id} sends (handleGetRun's anonymous wrapper struct, runs_policy.go)", () => {
    const runDetailOwnKeys = tsInterfaceKeys(runsTs, "RunDetail");
    expect(runDetailOwnKeys).toEqual([
      "ui_apps",
      "user_type_name",
      "model_provider_name",
      "model_provider_deleted",
      "created_via_name",
      "kept_until",
      "policy",
      "sync",
    ]);
  });

  it("RunSync and RunSyncOpen's keys are exactly runSyncView's and runSyncOpen's json tags (GET /runs/{id} sync.open, sshgateway_sync.go)", () => {
    const syncGo = readFileSync(join(root, "internal/api/sshgateway_sync.go"), "utf8");
    expect(tsInterfaceKeys(runsTs, "RunSync").sort()).toEqual(goJSONTags(syncGo, "runSyncView").sort());
    expect(tsInterfaceKeys(runsTs, "RunSyncOpen").sort()).toEqual(goJSONTags(syncGo, "runSyncOpen").sort());
  });

  it("RunEndWaitResult's keys are exactly runEndWaitResponse's json tags (PATCH /runs/{id}, run_end_wait.go)", () => {
    const endWaitGo = readFileSync(join(root, "internal/api/run_end_wait.go"), "utf8");
    expect(tsInterfaceKeys(runsTs, "RunEndWaitResult").sort()).toEqual(goJSONTags(endWaitGo, "runEndWaitResponse").sort());
  });

  it("CreateRunInput (the wizard-facing type) declares no key the Go DTO lacks", () => {
    const goTags = new Set(goJSONTags(clientGo, "CreateRunRequest"));
    const tsKeys = tsInterfaceKeys(runCreateTs, "CreateRunInput");
    expect(tsKeys.filter((k) => !goTags.has(k))).toEqual([]);
  });

  // components[] (#1914): the New Run access rows send it, built from a mirrored
  // type rather than a guess. The element is the SDK's ComponentRef, an
  // alias of types.ComponentRef, and its keys are exactly the Go tags.
  it("CreateRunRequest.components is []ComponentRef, and the TS ComponentRef keys are exactly its Go tags", () => {
    expect(clientGo).toMatch(/Components\s+\[\]ComponentRef\s+`json:"components,omitempty"`/);
    const typesGoSrc = readFileSync(join(root, "pkg/client/types.go"), "utf8");
    expect(typesGoSrc).toMatch(/ComponentRef\s*=\s*types\.ComponentRef/);
    const refGo = goJSONTags(readFileSync(join(root, "internal/types/component.go"), "utf8"), "ComponentRef");
    expect(refGo.sort()).toEqual(["access", "builtin", "id", "inline", "name", "org", "repos"]);
    const componentsTs = readFileSync(join(root, "ui/src/app/lib/types/components.ts"), "utf8");
    expect(tsInterfaceKeys(componentsTs, "ComponentRef").sort()).toEqual(refGo);
  });

  it("the console's response types carry the component facts both dry-run doors return", () => {
    const preflightGo = readFileSync(join(root, "internal/api/preflight.go"), "utf8");
    const previewGo = readFileSync(join(root, "internal/api/policy_preview_facts.go"), "utf8");
    for (const src of [preflightGo, previewGo]) expect(src).toMatch(/Components\s+\[\]componentFact\s+`json:"components,omitempty"`/);
    const previewTs = readFileSync(join(root, "ui/src/app/lib/types/policy-preview.ts"), "utf8");
    expect(tsInterfaceKeys(runsTs, "PreflightResult")).toContain("components");
    expect(tsInterfaceKeys(previewTs, "PolicyPreviewResult")).toContain("components");
    expect(runsTs).toMatch(/components\?: ComponentFact\[\];/);
    expect(previewTs).toMatch(/components\?: ComponentFact\[\];/);
  });
});

// D. F6-F14 — the same "documents the omissions" idiom (proven above for
// AgentRun), extended to five more flat structs. Still flat-only: goJSONTags
// does not handle nested braces, so an embedded response wrapper
// (grantView{types.CapabilityGrant; Inert},
// siteConfigPutResponse{types.SiteConfig;…}) is read off its own nested
// type, never folded into the base struct's tag list — these drifts are
// mostly embedded, which is why CapabilityGrant/RunPolicySpec below show
// full parity on the base struct even though the response bodies carry more.
describe("source parity — five more flat structs", () => {
  // ticket: F6-F14
  const root = repoRoot();
  const workspaceGo = readFileSync(join(root, "internal/types/workspace.go"), "utf8");
  // types.go plus types_capability.go (the CapabilityGrant/RoleMapping split,
  // #572's merge): concatenated so goJSONTags still finds a struct that moved
  // file when the split landed.
  const typesGoFull = readFileSync(join(root, "internal/types/types.go"), "utf8") +
    "\n" + readFileSync(join(root, "internal/types/types_capability.go"), "utf8");
  const siteConfigGo = readFileSync(join(root, "internal/types/site_config.go"), "utf8");
  const policyGo = readFileSync(join(root, "internal/types/policy.go"), "utf8");
  const workspacesTs = readFileSync(join(root, "ui/src/app/lib/types/workspaces.ts"), "utf8");
  const permissionsTs = readFileSync(join(root, "ui/src/app/lib/types/permissions.ts"), "utf8");
  const siteTs = readFileSync(join(root, "ui/src/app/lib/types/site.ts"), "utf8");
  const policyTs = readFileSync(join(root, "ui/src/app/lib/types/policy.ts"), "utf8");
  const auditTs = readFileSync(join(root, "ui/src/app/lib/types/audit.ts"), "utf8");
  const scmaccessGo = readFileSync(join(root, "internal/api/scmaccess.go"), "utf8");
  const setupTs = readFileSync(join(root, "ui/src/app/lib/types/setup.ts"), "utf8");

  it("documents (does not fail on) Go Workspace tags the TS mirror omits", () => {
    const goTags = goJSONTags(workspaceGo, "Workspace");
    const tsKeys = new Set(tsInterfaceKeys(workspacesTs, "Workspace"));
    const omitted = goTags.filter((t) => !tsKeys.has(t));
    // attachments: tier-3 attachment rows — the console reads the derived
    // `sources`/`effective_requirements` projections instead, never the raw
    // attachment table. built_profile_hash: the server's own build-cache key,
    // never read client-side. egress_edited_at: an internal heal marker
    // (ReconcileWorkspaceEgressDecisions) with no console consumer.
    expect(omitted.sort()).toEqual(["attachments", "built_profile_hash", "egress_edited_at"].sort());
  });

  it("CapabilityGrant: full parity with the BASE Go struct — `inert` is a known response-wrapper key", () => {
    const goTags = new Set(goJSONTags(typesGoFull, "CapabilityGrant"));
    expect(goTags.size).toBeGreaterThanOrEqual(8);
    // `inert` is NOT a types.CapabilityGrant field — it rides in on
    // internal/api/permissions.go's grantView{types.CapabilityGrant; Inert},
    // an EMBEDDED wrapper goJSONTags never sees (nested braces). Every other
    // TS key must be a real Go tag.
    const tsKeys = tsInterfaceKeys(permissionsTs, "CapabilityGrant");
    const unknown = tsKeys.filter((k) => k !== "inert" && !goTags.has(k));
    expect(unknown, "TS reads these off the grant payload but Go's base struct never writes them").toEqual([]);
    // …and nothing on the base struct is missing from the mirror.
    const omitted = [...goTags].filter((t) => !tsKeys.includes(t));
    expect(omitted).toEqual([]);
  });

  it("every Go SiteConfig tag is mirrored on the TS interface (R-04 — readonly fields now match)", () => {
    const goTags = goJSONTags(siteConfigGo, "SiteConfig");
    const tsKeys = new Set(tsInterfaceKeys(siteTs, "SiteConfig"));
    const omitted = goTags.filter((t) => !tsKeys.has(t));
    expect(omitted).toEqual([]);
  });

  it("RunPolicySpec: full parity, base struct — a future drift shows up as a diff either direction", () => {
    const goTags = goJSONTags(policyGo, "RunPolicySpec");
    expect(goTags.length).toBeGreaterThanOrEqual(17);
    const tsKeys = tsInterfaceKeys(policyTs, "RunPolicySpec");
    expect(new Set(goTags)).toEqual(new Set(tsKeys));
  });

  it("SiteConfig.components is the ComponentSettings block, and every Go tag of it is mirrored", () => {
    expect(siteConfigGo).toMatch(/Components\s+\*ComponentSettings\s+`json:"components,omitempty"`/);
    expect(siteTs).toMatch(/components\?: ComponentSettings;/);
    const goTags = goJSONTags(siteConfigGo, "ComponentSettings");
    expect(goTags.length).toBe(3);
    expect(new Set(tsInterfaceKeys(siteTs, "ComponentSettings"))).toEqual(new Set(goTags));
  });

  it("RunPolicySpec.github_capabilities is mirrored beside azure_devops_capabilities", () => {
    expect(goJSONTags(policyGo, "RunPolicySpec")).toContain("github_capabilities");
    expect(tsInterfaceKeys(policyTs, "RunPolicySpec")).toEqual(expect.arrayContaining(["azure_devops_capabilities", "github_capabilities"]));
  });

  it("every Go AuditEvent tag is mirrored on the TS interface (prev_hash/row_hash closed)", () => {
    const goTags = goJSONTags(typesGoFull, "AuditEvent");
    expect(goTags.length).toBeGreaterThanOrEqual(10);
    const tsKeys = new Set(tsInterfaceKeys(auditTs, "AuditEvent"));
    const omitted = goTags.filter((t) => !tsKeys.has(t));
    expect(omitted).toEqual([]);
    // …and nothing on the TS side claims a field Go never writes.
    const unknown = [...tsKeys].filter((k) => !goTags.includes(k));
    expect(unknown, "TS reads these off the audit payload but Go never writes them").toEqual([]);
  });

  // review finding F4: TS SCMAccess still carried `row_id`, which Go dropped
  // in 84cd08e1 ("NO ROW ID", scmaccess.go's own doc comment), and lacked
  // Go's `kind`. Full parity, base struct — either direction of drift fails.
  it("every Go SCMAccess tag is mirrored on the TS interface, and nothing extra (row_id dropped, kind added)", () => {
    // ticket: F4
    const goTags = goJSONTags(scmaccessGo, "SCMAccess");
    expect(goTags.length).toBeGreaterThanOrEqual(4);
    const tsKeys = tsInterfaceKeys(setupTs, "SCMAccess");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });
});

// #510-F8 — the autonomy wire types (0.8 #97/#93) were hand-mirrored in
// governance.ts with no entry in this suite: a Go rename of any of the nine
// AutonomyRubric fields, or a drift on AutonomyPosture/AutonomyResolution,
// would have shown up only as a runtime `undefined`. Full parity, base
// structs — either direction of drift fails, same discipline as
// RunPolicySpec/SCMAccess above.
describe("source parity — autonomy wire types (#510-F8)", () => {
  const root = repoRoot();
  const governanceGo = readFileSync(join(root, "internal/types/governance.go"), "utf8");
  const governanceTs = readFileSync(join(root, "ui/src/app/lib/api/governance.ts"), "utf8");

  it("AutonomyRubric: full parity — the nine level caps and the guardrail lock", () => {
    const goTags = goJSONTags(governanceGo, "AutonomyRubric");
    expect(goTags.length).toBe(10);
    const tsKeys = tsInterfaceKeys(governanceTs, "AutonomyRubric");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });

  it("AutonomyPosture: full parity — the three-axis shape", () => {
    const goTags = goJSONTags(governanceGo, "AutonomyPosture");
    expect(goTags.length).toBe(3);
    const tsKeys = tsInterfaceKeys(governanceTs, "AutonomyPosture");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });

  it("AutonomyResolution: full parity — level, posture, bound_by", () => {
    const goTags = goJSONTags(governanceGo, "AutonomyResolution");
    expect(goTags.length).toBe(3);
    const tsKeys = tsInterfaceKeys(governanceTs, "AutonomyResolution");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });
});
