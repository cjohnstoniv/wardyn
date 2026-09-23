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
//      pkg/client.CreateRunRequest (client.go:178-300) that the console can
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

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { runs } from "./runs";
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
// One entry per Go DTO field the UI forwards (client.go:178-300 ∩ runs.ts:70-116).
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
  workspaces: [{ workspace_id: "ws-1", enabled_optional: ["egress:api.stripe.com"], read_only: true }],
  workspace_id: "22222222-2222-2222-2222-222222222222",
  integration_id: "anthropic_api_key",
  // The member's drive request (D5): forwarded verbatim by runWireBody — a
  // path never rides here, only {enabled, read_only}.
  drive: { enabled: true, read_only: false },
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
};

// Go DTO JSON tags the console NEVER sends (CLI-only — cmd/wardyn/commands.go:96-103).
// If the whitelist starts forwarding one of these, or the Go DTO drops one,
// this list must change in the same commit.
const UI_NEVER_SENDS = new Set(["devcontainer_repo", "devcontainer_ref"]);

// ui_apps used to sit on this set as a TS AgentRun key with no Go AgentRun
// json tag (handleGetRun's anonymous wrapper struct, runs_policy.go:172-175,
// GET /runs/{id} only) — it now lives on RunDetail instead (see the RunDetail
// test below), so AgentRun itself needs no wrapper-key exclusion.

// A. Every forwarded field reaches both doors with the value the caller set.
describe("runWireBody — every console-settable DTO field reaches the wire (F8 probe)", () => {
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

  it("createRun and preflightRun send byte-identical bodies for the same input", async () => {
    await runs.createRun(fullInput);
    const a = String(fetchMock.mock.calls[0][1]?.body);
    fetchMock.mockClear();
    await runs.preflightRun(fullInput);
    const b = String(fetchMock.mock.calls[0][1]?.body);
    expect(a).toBe(b);
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
  it("H3a: an unrecognised requested class is replaced by the floor (server 400 is masked)", async () => {
    await runs.createRun({ ...base, confinement_class: "cc2" as never, inline_policy: policyWithFloor("CC2") });
    expect(sentBody().confinement_class).toBe("CC2");
  });

  it("H3b: an unrecognised FLOOR disables the clamp entirely", async () => {
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

describe("source parity — Go wire tags vs the TS mirror (F8 probe)", () => {
  const root = repoRoot();
  const clientGo = readFileSync(join(root, "pkg/client/client.go"), "utf8");
  const typesGo = readFileSync(join(root, "internal/types/types.go"), "utf8");
  const runsTs = readFileSync(join(root, "ui/src/app/lib/types/runs.ts"), "utf8");

  it("every CreateRunRequest json tag is forwarded by runWireBody or on the UI-never-sends list", async () => {
    const goTags = goJSONTags(clientGo, "CreateRunRequest");
    expect(goTags.length).toBeGreaterThanOrEqual(17); // stale-regex guard (Go test does the same)
    await runs.createRun(fullInput);
    const wireKeys = new Set(Object.keys(sentBody()));
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
    await runs.createRun(fullInput);
    const stray = Object.keys(sentBody()).filter((k) => !goTags.has(k));
    expect(stray, "keys the server will reject with 400 'unknown field' (helpers.go:393)").toEqual([]);
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

  it("every Go AgentRun tag is mirrored on the TS interface (F8: agent_exec_id/auto_stop_after_sec/source_id closed)", () => {
    const goTags = goJSONTags(typesGo, "AgentRun");
    const tsKeys = new Set(tsInterfaceKeys(runsTs, "AgentRun"));
    const omitted = goTags.filter((t) => !tsKeys.has(t));
    expect(omitted).toEqual([]);
  });

  it("RunDetail adds exactly ui_apps over AgentRun — the ONE field only GET /runs/{id} sends " +
    "(handleGetRun's anonymous wrapper struct, runs_policy.go)", () => {
    const runDetailOwnKeys = tsInterfaceKeys(runsTs, "RunDetail");
    expect(runDetailOwnKeys).toEqual(["ui_apps"]);
  });

  it("CreateRunInput (the wizard-facing type) declares no key the Go DTO lacks", () => {
    const goTags = new Set(goJSONTags(clientGo, "CreateRunRequest"));
    const tsKeys = tsInterfaceKeys(runsTs, "CreateRunInput");
    expect(tsKeys.filter((k) => !goTags.has(k))).toEqual([]);
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
describe("source parity — five more flat structs (F6-F14)", () => {
  const root = repoRoot();
  const workspaceGo = readFileSync(join(root, "internal/types/workspace.go"), "utf8");
  const typesGoFull = readFileSync(join(root, "internal/types/types.go"), "utf8");
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
  it("every Go SCMAccess tag is mirrored on the TS interface, and nothing extra (F4: row_id dropped, kind added)", () => {
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

  it("AutonomyRubric: full parity — the nine closed fields", () => {
    const goTags = goJSONTags(governanceGo, "AutonomyRubric");
    expect(goTags.length).toBe(9);
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
