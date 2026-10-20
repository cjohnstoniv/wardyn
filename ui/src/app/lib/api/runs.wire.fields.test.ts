/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F8-ui-types-mirror probe: pins intentToWire's wire-forwarding to the Go DTO
// (imports `./runs` and `./core` relative to this directory).
//
// Run (from the repo root, no PG needed - fetch is stubbed):
//   cd ui && pnpm vitest run src/app/lib/api/runs.wire.fields.test.ts
//
// What it pins, in three layers:
//   A. The three request doors send the body intentToWire built, byte for byte,
//      to the route the Go router registers.
//   B. Source parity, from the TS side - the counterpart of Go's
//      TestTerminalRunStates_UIParity (internal/types/terminal_parity_test.go):
//      every JSON tag on the Go CreateRunRequest is carried by intentToWire, sent
//      by the Name dialog, or excluded by the template field registry (so the
//      never-sends list is empty); every key the TS AgentRun interface
//      (types/runs.ts) declares is a JSON tag on Go's types.AgentRun or on
//      handleGetRun's wrapper (runs_policy.go). A Go rename (e.g. failure_hint
//      -> failure_reason) fails here instead of as a runtime `undefined` the
//      console renders as "-". The Go side is read from source with the same
//      regex discipline the Go parity test uses; the repo root is found by
//      walking up to go.mod so the file works from ui/ (vitest's cwd) or
//      anywhere under it.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { runs } from "./runs";
import { TEMPLATE_REQUEST_FIELDS } from "../template-fields";
import { intentToWire, POLICY_CARRIER } from "../../components/screens/new-run-flow/draft/intent-to-wire";
import type { RunIntent } from "../../components/screens/new-run-flow/draft/run-intent";

// Shared fetch stub.
let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(() => {
  // A FRESH Response per call: a Response body can only be read once.
  fetchMock = vi.fn().mockImplementation(async () =>
    new Response(JSON.stringify({ id: "run_1" }), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => vi.unstubAllGlobals());

// Go DTO JSON tags the console never sends. Emptied at the cutover: a tag the
// intent cannot carry is excluded in the template field registry, with a reason.
const UI_NEVER_SENDS = new Set<string>();

// One workspace entry with every key the Go WorkspaceSelection declares.
const workspaceEntry = { workspace_id: "ws-1", enabled_optional: ["egress:api.stripe.com"], read_only: true, target: "/sandbox/work", repos: [{ repo: "acme/web", ref: "main" }] };

// An intent that sets every field the registry carries, with a value the
// serializer only has to pass through. inline_policy travels as policy_overlay.
function sampleIntent(): RunIntent {
  const intent: Record<string, unknown> = {};
  for (const r of TEMPLATE_REQUEST_FIELDS) if (!r.excluded && !r.pending) intent[r.name] = r.name === "inline_policy" ? {} : 1;
  return intent as RunIntent;
}

// A. The three request doors send the serializer's body untouched.
describe("the request doors send the body intentToWire built", () => {
  const { body } = intentToWire({ shape: "agent_task", workspaces: [workspaceEntry], resources: { disk_mib: 1 } }, { title: "Refund flow", description: "" });
  const sent = () => ({ path: String(fetchMock.mock.calls[0][0]), body: String(fetchMock.mock.calls[0][1]?.body) });

  it.each([
    ["createRunWire", "/api/v1/runs"],
    ["preflightRunWire", "/api/v1/runs/preflight"],
    ["previewRunPolicyWire", "/api/v1/runs/policy-preview"],
  ] as const)("%s posts the body byte for byte to %s", async (door, path) => {
    await runs[door](body);
    expect(sent()).toEqual({ path, body: JSON.stringify(body) });
    expect(fetchMock.mock.calls[0][1]?.method).toBe("POST");
  });

  it("calls the registered policy-preview route", () => {
    const routes = readFileSync(join(repoRoot(), "internal/api/routes.go"), "utf8");
    expect(/r\.Post\("([^"]+)", s\.handlePolicyPreview\)/.exec(routes)?.[1]).toBe("/runs/policy-preview");
  });

  it("keeps the setup rows a preflight model_credential refusal carries, and none on another refusal", async () => {
    const row = { id: "backend:CC2", kind: "backend", label: "Sandbox barrier", required_by: "x", status: "missing" };
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ error: "sign in", reason: "model_credential", provider: "p", setup_items: [row] }), { status: 422 }));
    await expect(runs.preflightRunWire(body)).rejects.toMatchObject({ status: 422, reason: "model_credential", provider: "p", setupItems: [row] });
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ error: "off", reason: "model_provider_unavailable" }), { status: 422 }));
    const other = await runs.preflightRunWire(body).catch((e: unknown) => e);
    expect(other).toMatchObject({ status: 422, reason: "model_provider_unavailable" });
    expect((other as { setupItems?: unknown }).setupItems).toBeUndefined();
  });

  it("preserves Retry-After on a preview refusal for the bounded preview scheduler", async () => {
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ error: "limited" }), { status: 429, headers: { "Retry-After": "3" } }));
    await expect(runs.previewRunPolicyWire(body)).rejects.toMatchObject({ status: 429, message: "limited", retryAfter: "3" });
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ error: "limited" }), { status: 429 }));
    await expect(runs.previewRunPolicyWire(body)).rejects.toMatchObject({ status: 429, retryAfter: undefined });
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

  it("every CreateRunRequest json tag is carried by intentToWire, sent by the Name dialog, or excluded by the registry", () => {
    const goTags = goJSONTags(clientGo, "CreateRunRequest");
    expect(goTags.length).toBeGreaterThanOrEqual(17); // stale-regex guard (Go test does the same)
    const { body, unsupported } = intentToWire(sampleIntent(), { title: "t", description: "d" });
    expect(unsupported, "a carried registry row the serializer cannot send").toEqual([]);
    // inline_policy is the document's name for the overlay; it travels as policy_overlay.
    const carried = TEMPLATE_REQUEST_FIELDS.filter((r) => !r.excluded && !r.pending).map((r) => r.name);
    for (const name of carried) expect(body, name).toHaveProperty(name === "inline_policy" ? POLICY_CARRIER : name);
    const accounted = new Set([...carried, ...TEMPLATE_REQUEST_FIELDS.filter((r) => r.excluded).map((r) => r.name)]);
    const missing = goTags.filter((t) => !accounted.has(t) && !UI_NEVER_SENDS.has(t));
    expect(
      missing,
      "Go DTO fields the console can never send: add a registry row (internal/api/template_fields.go) " +
        "that carries or excludes them, and regenerate the golden",
    ).toEqual([]);
    for (const t of UI_NEVER_SENDS) expect(goTags, `UI_NEVER_SENDS names ${t}, which is no longer a Go DTO field`).toContain(t);
  });

  it("every key intentToWire emits is a CreateRunRequest json tag (a stray key is a strict-decode 400)", () => {
    const goTags = new Set(goJSONTags(clientGo, "CreateRunRequest"));
    const stray = Object.keys(intentToWire(sampleIntent(), { title: "t", description: "d" }).body).filter((k) => !goTags.has(k));
    expect(stray, "keys the server will reject with 400 'unknown field' (helpers.go:393)").toEqual([]);
  });

  it("every RunResources json tag reaches the wire when set alone (a disk-only block is not dropped)", () => {
    for (const tag of goJSONTags(clientGo, "RunResources")) {
      const { body, unsupported } = intentToWire({ resources: { [tag]: 1 } });
      expect(unsupported).toEqual([]);
      expect(body.resources, `resources.${tag} is dropped by intentToWire`).toEqual({ [tag]: 1 });
    }
  });

  it("WorkspaceSelection: the TS wire entry's keys are exactly Go's json tags", () => {
    const goTags = goJSONTags(clientGo, "WorkspaceSelection").sort();
    const sent = Object.keys(workspaceEntry).sort();
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

  it("RunDetail adds exactly ui_apps, user_type_name, the model provider's name and deleted flag, the launching portal's name, the pool's name, the ended run's kept_until, the run's policy, its open sync sessions and its agents over AgentRun — " +
    "the fields only GET /runs/{id} sends (handleGetRun's anonymous wrapper struct, runs_policy.go)", () => {
    const runDetailOwnKeys = tsInterfaceKeys(runsTs, "RunDetail");
    expect(runDetailOwnKeys).toEqual([
      "ui_apps",
      "user_type_name",
      "model_provider_name",
      "model_provider_deleted",
      "created_via_name",
      "runner_pool_name",
      "kept_until",
      "policy",
      "sync",
      "agents",
    ]);
  });

  it("RunDetailAgent's keys are exactly runDetailAgent's json tags (GET /runs/{id} agents, run_multi_agent.go)", () => {
    const multiGo = readFileSync(join(root, "internal/api/run_multi_agent.go"), "utf8");
    expect(tsInterfaceKeys(runsTs, "RunDetailAgent").sort()).toEqual(goJSONTags(multiGo, "runDetailAgent").sort());
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

  it("CreateRunInput (the request type RunIntent is built on) declares no key the Go DTO lacks", () => {
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
    expect(refGo.sort()).toEqual(["access", "builtin", "id", "inline", "mcp_harnesses", "name", "org", "repos"]);
    const componentsTs = readFileSync(join(root, "ui/src/app/lib/types/components.ts"), "utf8");
    expect(tsInterfaceKeys(componentsTs, "ComponentRef").sort()).toEqual(refGo);
  });

  it("the console's response types carry the component facts both dry-run doors return", () => {
    const preflightGo = readFileSync(join(root, "internal/api/preflight.go"), "utf8");
    const previewGo = readFileSync(join(root, "internal/api/policy_preview_facts.go"), "utf8");
    for (const src of [preflightGo, previewGo]) expect(src).toMatch(/Components\s+\[\]componentFact\s+`json:"components,omitempty"`/);
    const previewTs = readFileSync(join(root, "ui/src/app/lib/types/policy-preview.ts"), "utf8");
    const preflightTs = readFileSync(join(root, "ui/src/app/lib/types/preflight.ts"), "utf8");
    expect(tsInterfaceKeys(preflightTs, "PreflightResult")).toContain("components");
    expect(tsInterfaceKeys(previewTs, "PolicyPreviewResult")).toContain("components");
    expect(preflightTs).toMatch(/components\?: ComponentFact\[\];/);
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
