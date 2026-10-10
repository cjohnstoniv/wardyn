/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// T-69 — Go->TS wire parity for the DTOs the F8 console-e2e audit named that
// had no source-parity probe yet: SetupStatus, ApprovalRequest, and the
// attach-mode control
// frame (attachModeMsg + its nested attachHolderView, `attach_holder.go`
// 215-263 — the shape ui/e2e/attach-stub.ts hand-builds against). SiteConfig,
// AgentRun, RunPolicySpec, AuditEvent, SCMAccess, CapabilityGrant and Me
// already have this probe (runs.wire.fields.test.ts §C/D, health.test.ts
// F010) — this file does not repeat them.
//
// Same technique as those siblings: read the Go struct and the TS interface
// as TEXT and diff their json-tag / property-name sets, rather than
// hand-retyping either side (a hand-typed list drifts silently; this fails
// loudly the moment either source changes). goJSONTags/tsInterfaceKeys are
// copied from runs.wire.fields.test.ts's own copy (health.test.ts and
// approvals.wire.test.ts each carry their own too) — each parity file stays a
// standalone probe runnable on its own, per the header comment convention
// those files already establish.
//
// tsMembers extends that idiom with brace-depth tracking: SetupStatus
// declares several fields as INLINE anonymous object types
// (`auth: { mode: ...; ... }`, `platform: { os: string; wsl: boolean }`)
// rather than named interfaces, so the flat line-by-line regex the other files
// use would misread "mode"/"driver"/etc. as SetupStatus's OWN keys. The same
// walk hands back each inline body, so those nested shapes are pinned against
// their Go structs (SetupAuth, SetupRunner, ...) too. Comments are stripped
// first so example JSON or prose braces in a doc comment can't shift the depth
// count either.
//
// Run: cd ui && pnpm vitest run src/app/lib/types/wire-parity.test.ts

import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

// The json tag names of one Go struct body (runs.wire.fields.test.ts's copy).
function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  const tags: string[] = [];
  for (const t of m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)) {
    if (t[1] !== "-") tags.push(t[1]);
  }
  return tags;
}

function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");
}

// The top-level members of one TS object-type body, each mapped to its inline
// object-type body when the member's type IS an inline `{ ... }` literal (null
// otherwise). Members split at `;` or a newline, but only at brace depth 0, so
// a nested key never reads as this body's own and a one-line
// `platform: { os: string; wsl: boolean }` stays one member.
function tsMembers(body: string): Map<string, string | null> {
  const members = new Map<string, string | null>();
  let depth = 0;
  let start = 0;
  for (let i = 0; i <= body.length; i++) {
    const ch = body[i];
    if (ch === "{") depth++;
    else if (ch === "}") depth--;
    else if (depth === 0 && (i === body.length || ch === ";" || ch === "\n")) {
      const seg = body.slice(start, i);
      const k = /^\s*(?:readonly\s+)?([A-Za-z_][A-Za-z0-9_]*)\??\s*:\s*(\{)?/.exec(seg);
      if (k) members.set(k[1], k[2] ? seg.slice(k[0].length, seg.lastIndexOf("}")) : null);
      start = i + 1;
    }
  }
  return members;
}

function tsInterfaceBody(src: string, name: string): string {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${name} not found`);
  return m[1];
}

// Like the other files' tsInterfaceKeys, but only TOP-LEVEL properties (see
// the file header for why SetupStatus needs this).
function tsInterfaceTopKeys(src: string, name: string): string[] {
  return [...tsMembers(tsInterfaceBody(src, name)).keys()];
}

// The keys of an inline object type declared as one member of an interface —
// SetupStatus's `auth: { ... }`, `runner: { ... }` and the rest.
function tsInlineKeys(src: string, iface: string, member: string): string[] {
  const body = tsMembers(tsInterfaceBody(src, iface)).get(member);
  if (body == null) throw new Error(`${iface}.${member} is not an inline object type`);
  return [...tsMembers(body).keys()];
}

describe("source parity — Go DTOs vs their TS mirrors (T-69)", () => {
  const root = repoRoot();
  const setupGo = readFileSync(join(root, "internal/api/setup.go"), "utf8");
  const setupChecksGo = readFileSync(join(root, "internal/api/setup_checks.go"), "utf8");
  const harnessToolGo = readFileSync(join(root, "internal/api/setup_integrations.go"), "utf8");
  const attachGo = readFileSync(join(root, "internal/api/attach_holder.go"), "utf8");
  const typesGo = readFileSync(join(root, "internal/types/types.go"), "utf8");
  const setupTs = readFileSync(join(root, "ui/src/app/lib/types/setup.ts"), "utf8");
  const runsTs = readFileSync(join(root, "ui/src/app/lib/types/runs.ts"), "utf8");
  const approvalsTs = readFileSync(join(root, "ui/src/app/lib/types/approvals.ts"), "utf8");

  it("SetupStatus: every Go tag is mirrored, and every TS key besides the UI-only `unreachable` is a Go tag", () => {
    const goTags = goJSONTags(setupGo, "SetupStatus");
    expect(goTags.length).toBeGreaterThanOrEqual(20); // stale-regex guard
    const tsKeys = tsInterfaceTopKeys(setupTs, "SetupStatus");
    expect(tsKeys.length).toBeGreaterThanOrEqual(20);
    const omitted = goTags.filter((t) => !tsKeys.includes(t));
    expect(omitted, "handleSetupStatus sends these but the TS SetupStatus mirror cannot read them").toEqual([]);
    // `unreachable` is the one documented exception (setup.ts's own doc
    // comment: "UI-ONLY, never on the wire" — api.getSetupStatus()'s network
    // -error fallback, never emitted by handleSetupStatus).
    const unknown = tsKeys.filter((k) => k !== "unreachable" && !goTags.includes(k));
    expect(unknown, "TS claims these SetupStatus keys but Go never sends them").toEqual([]);
  });

  // SetupStatus's nested DTOs — most of the /setup/status surface. The test
  // above proves only that `auth`, `checks`, ... exist; these pin what is inside
  // them (SetupCheck.blocking is the 0.7.8 server-side setup gate). Named TS
  // interfaces first, then the members setup.ts declares as inline types.
  it.each([
    ["SetupCheck", setupChecksGo],
    ["SetupProvider", setupGo],
    ["SetupHarnessTool", harnessToolGo],
  ])("%s: full parity with the TS mirror of the same name", (name, goSrc) => {
    const goTags = goJSONTags(goSrc, name);
    expect(goTags.length).toBeGreaterThan(0);
    expect(new Set(tsInterfaceTopKeys(setupTs, name))).toEqual(new Set(goTags));
  });

  it.each([
    ["SetupAuth", "auth"],
    ["SetupRunner", "runner"],
    ["SetupSecrets", "secrets"],
    ["SetupAgeKey", "age_key"],
    ["SetupPlatform", "platform"],
  ])("%s: full parity with SetupStatus.%s's inline TS type", (goName, member) => {
    const goTags = goJSONTags(setupGo, goName);
    expect(goTags.length).toBeGreaterThan(0);
    expect(new Set(tsInlineKeys(setupTs, "SetupStatus", member))).toEqual(new Set(goTags));
  });

  it("SetupProviderAccess (`provider_access`): full parity with the TS mirror", () => {
    const providerAccessGo = readFileSync(join(root, "internal/api/provider_access.go"), "utf8");
    const goTags = goJSONTags(providerAccessGo, "SetupProviderAccess");
    expect(goTags.length).toBeGreaterThanOrEqual(5);
    expect(new Set(tsInterfaceTopKeys(setupTs, "SetupProviderAccess"))).toEqual(new Set(goTags));
  });

  it("ApprovalRequest (`Approval`): full parity with the TS mirror", () => {
    const goTags = goJSONTags(typesGo, "ApprovalRequest");
    expect(goTags.length).toBeGreaterThanOrEqual(10);
    const tsKeys = tsInterfaceTopKeys(approvalsTs, "ApprovalRequest");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });

  // AttachModeFrame — the ONE server->client control frame on the attach
  // WebSocket (attach_holder.go:215-263), hand-mirrored in ui/e2e/attach-stub.ts
  // against this same Go source rather than a shared type, per that file's own
  // header comment. Two Go structs, two TS interfaces.
  it("attachModeMsg (`AttachModeFrame`): full parity with the TS AttachModeMsg mirror", () => {
    const goTags = goJSONTags(attachGo, "attachModeMsg");
    expect(goTags.length).toBeGreaterThanOrEqual(3);
    const tsKeys = tsInterfaceTopKeys(runsTs, "AttachModeMsg");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });

  it("attachHolderView (nested in AttachModeFrame as `holder`): full parity with the TS AttachHolder mirror", () => {
    const goTags = goJSONTags(attachGo, "attachHolderView");
    expect(goTags.length).toBeGreaterThanOrEqual(6);
    const tsKeys = tsInterfaceTopKeys(runsTs, "AttachHolder");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });

  // GET /model-providers' body (#970): the stored block (types.ModelProviders,
  // embedded) plus connected_people, mirrored as ModelProvidersRead extending
  // ModelProviders.
  it("modelProvidersRead (GET /model-providers): full parity with the TS ModelProvidersRead mirror", () => {
    const siteTs = readFileSync(join(root, "ui/src/app/lib/types/site.ts"), "utf8");
    const readGo = readFileSync(join(root, "internal/api/model_providers_api.go"), "utf8");
    const blockGo = readFileSync(join(root, "internal/types/model_provider.go"), "utf8");
    expect(readGo).toMatch(/type modelProvidersRead struct \{\n\ttypes\.ModelProviders\n/);
    expect(siteTs).toMatch(/export interface ModelProvidersRead extends ModelProviders \{/);
    expect(new Set(tsInterfaceTopKeys(siteTs, "ModelProvidersRead"))).toEqual(new Set(goJSONTags(readGo, "modelProvidersRead")));
    expect(new Set(tsInterfaceTopKeys(siteTs, "ModelProviders"))).toEqual(new Set(goJSONTags(blockGo, "ModelProviders")));
  });

  // #1215: the site config's branding block — one tag today (logo_path), so it
  // cannot ride the it.each below, whose stale-regex guard wants two.
  it("SiteBranding (`branding` in site config): full parity with the TS mirror", () => {
    const goTags = goJSONTags(readFileSync(join(root, "internal/types/site_config.go"), "utf8"), "SiteBranding");
    expect(goTags).toEqual(["logo_path"]);
    const tsKeys = tsInterfaceTopKeys(readFileSync(join(root, "ui/src/app/lib/types/site.ts"), "utf8"), "SiteBranding");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });

  // withheld_scm_hosts rows: three tags, no Go struct beyond types.WithheldScmHost.
  it("WithheldScmHost (`withheld_scm_hosts` in site config): full parity with the TS mirror", () => {
    const goTags = goJSONTags(readFileSync(join(root, "internal/types/site_config.go"), "utf8"), "WithheldScmHost");
    expect(goTags).toEqual(["host", "provider_id", "provider_kind"]);
    const tsKeys = tsInterfaceTopKeys(readFileSync(join(root, "ui/src/app/lib/types/site.ts"), "utf8"), "WithheldScmHost");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });

  // 0.8.6 fleet-fl4: GET /admin/runs/capacity. The store structs (RunCapacityRunner
  // embeds RunCapacitySums, so its own tag set is `basis`, and the TS interface
  // extends the sums), then the handler's envelope: an anonymous struct embedding
  // store.RunCapacity, so the response's keys are RunCapacity's plus its two.
  it.each([
    ["RunCapacitySums", "RunCapacitySums"],
    ["RunCapacityRunner", "RunCapacityRunner"],
    ["RunCapacityOwner", "RunCapacityOwner"],
    ["RunCapacityAge", "RunCapacityAge"],
    ["RunCapacityUnschedulable", "RunCapacityUnschedulable"],
    ["RunCapacity", "RunCapacity"],
  ])("%s (GET /admin/runs/capacity): full parity with the TS mirror", (goName, tsName) => {
    const goTags = goJSONTags(readFileSync(join(root, "internal/store/store_run_capacity.go"), "utf8"), goName);
    expect(goTags.length).toBeGreaterThanOrEqual(1);
    expect(new Set(tsInterfaceTopKeys(runsTs, tsName))).toEqual(new Set(goTags));
  });

  it("RunCapacityResponse (GET /admin/runs/capacity): the handler adds generated_at and basis to RunCapacity", () => {
    const handlerGo = readFileSync(join(root, "internal/api/runs_capacity.go"), "utf8");
    expect(handlerGo).toMatch(/GeneratedAt time\.Time\s+`json:"generated_at"`/);
    expect(handlerGo).toMatch(/Basis\s+string\s+`json:"basis"`\n\t\tstore\.RunCapacity\n/);
    expect(new Set(tsInterfaceTopKeys(runsTs, "RunCapacityResponse"))).toEqual(new Set(["generated_at", "basis"]));
  });

  // #1215: GET /branding/settings is the public subset (brandingPublic, embedded)
  // plus brandingSettings' own keys, which is everything the TS Branding mirror
  // declares — logo_from_file, the read-only file-delivered mark, included.
  it("brandingSettings (GET /branding/settings): full parity with the TS Branding mirror", () => {
    const brandingGo = readFileSync(join(root, "internal/api/branding.go"), "utf8");
    expect(brandingGo).toMatch(/type brandingSettings struct \{\n\tbrandingPublic\n/);
    const goTags = [...goJSONTags(brandingGo, "brandingPublic"), ...goJSONTags(brandingGo, "brandingSettings")];
    expect(goTags).toContain("logo_from_file");
    const tsKeys = tsInterfaceTopKeys(readFileSync(join(root, "ui/src/app/lib/branding.ts"), "utf8"), "Branding");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });

  // #923's two reads: the Images tab's catalog row and the Available to view.
  it.each([
    ["BaseImageEntry", "internal/types/workspace_contract.go", "BaseImageEntry", "ui/src/app/lib/types/workspaces.ts"],
    ["availabilityView", "internal/api/permissions_availability.go", "AvailabilityView", "ui/src/app/lib/types/permissions.ts"],
    // #1428: the Azure DevOps row's `entra` block, which now carries the token
    // lifetimes (pat_max_hours, pat_max_days) the console writes.
    ["ADOEntraConfig", "internal/types/workspace_provider.go", "ADOEntraConfig", "ui/src/app/lib/types/site.ts"],
    // #1428: the per-person token console's reads.
    ["adoOrgCheckResult", "internal/api/ado_pat_orgcheck.go", "ADOOrgCheck", "ui/src/app/lib/types/ado-pat.ts"],
    ["adoPATRefusal", "internal/api/ado_pat_refusal.go", "ADOPATRefusal", "ui/src/app/lib/types/ado-pat.ts"],
    ["adoRunToken", "internal/api/ado_pat_console.go", "ADORunToken", "ui/src/app/lib/types/ado-pat.ts"],
    ["ADOPATAccess", "internal/api/ado_pat_console.go", "ADOPATAccess", "ui/src/app/lib/types/ado-pat.ts"],
  ])("%s: full parity with the TS mirror", (goName, goFile, tsName, tsFile) => {
    const goTags = goJSONTags(readFileSync(join(root, goFile), "utf8"), goName);
    expect(goTags.length).toBeGreaterThanOrEqual(2); // stale-regex guard (ADOPATAccess has two tags)
    const tsKeys = tsInterfaceTopKeys(readFileSync(join(root, tsFile), "utf8"), tsName);
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });
  // GET /api/v1/runs/{id}/policy (#1425): the SDK's RunPolicyView and its three
  // nested DTOs, the server struct they are pinned to, and the closed value sets.
  it.each([
    ["RunPolicyView", "runPolicyResponse", "RunPolicyView"],
    ["RunPolicySource", "runPolicySource", "RunPolicySource"],
    ["RunPolicyChange", "runPolicyChange", "RunPolicyChange"],
    ["StoredPolicyNow", "storedPolicyNow", "StoredPolicyNow"],
  ])("%s: full parity between pkg/client, the server struct and the TS mirror", (sdkName, serverName, tsName) => {
    const sdkGo = readFileSync(join(root, "pkg/client/run_policy.go"), "utf8");
    const serverGo = readFileSync(join(root, "internal/api/run_policy_view.go"), "utf8");
    const sdkTags = goJSONTags(sdkGo, sdkName);
    expect(sdkTags.length).toBeGreaterThanOrEqual(3);
    expect(new Set(goJSONTags(serverGo, serverName))).toEqual(new Set(sdkTags));
    expect(new Set(tsInterfaceTopKeys(runsTs, tsName))).toEqual(new Set(sdkTags));
  });

  it("RunPolicyView value sets: source kinds, causes and stored_policy_now states match the server", () => {
    const serverGo = readFileSync(join(root, "internal/api/run_policy_view.go"), "utf8");
    const explainGo = readFileSync(join(root, "internal/api/run_policy_explain.go"), "utf8");
    const consts = (src: string, prefix: string) =>
      new Set([...src.matchAll(new RegExp(`\\b${prefix}[A-Za-z]+\\s*=\\s*"([a-z_]+)"`, "g"))].map((m) => m[1]));
    const tsUnion = (name: string) => {
      const m = new RegExp(`export type ${name} =([^;]+);`).exec(stripComments(runsTs));
      if (!m) throw new Error(`type ${name} not found`);
      return new Set([...m[1].matchAll(/"([a-z_]+)"/g)].map((x) => x[1]));
    };
    expect(tsUnion("RunPolicySourceKind")).toEqual(consts(serverGo, "policyKind"));
    expect(tsUnion("RunPolicyCause")).toEqual(consts(explainGo, "cause"));
    expect(tsUnion("RunPolicyState")).toEqual(consts(serverGo, "policyView"));
    const states = new Set([...serverGo.matchAll(/storedPolicyNow\{State: "([a-z]+)"/g)].map((m) => m[1]));
    for (const m of serverGo.matchAll(/now\.State = "([a-z]+)"/g)) states.add(m[1]);
    const tsStates = /state: ([^;]+);/.exec(tsInterfaceBody(runsTs, "StoredPolicyNow"))![1];
    expect(new Set([...tsStates.matchAll(/"([a-z]+)"/g)].map((m) => m[1]))).toEqual(states);
  });

  // ADOPATAccess.last_token is an inline object type; adoLastToken is its Go struct.
  it("adoLastToken: full parity with the TS ADOPATAccess.last_token inline type", () => {
    const goTags = goJSONTags(readFileSync(join(root, "internal/api/ado_pat_console.go"), "utf8"), "adoLastToken");
    expect(goTags.length).toBeGreaterThanOrEqual(2);
    const adoPatTs = readFileSync(join(root, "ui/src/app/lib/types/ado-pat.ts"), "utf8");
    expect(new Set(tsInlineKeys(adoPatTs, "ADOPATAccess", "last_token"))).toEqual(new Set(goTags));
  });

  // GET /runs/{id}/output: RunOutput mirrors runOutputResponse.
  it("runOutputResponse (GET /runs/{id}/output): full parity with the TS RunOutput mirror", () => {
    const goTags = goJSONTags(readFileSync(join(root, "internal/api/run_output.go"), "utf8"), "runOutputResponse");
    expect(goTags.length).toBeGreaterThanOrEqual(7);
    const outTs = readFileSync(join(root, "ui/src/app/lib/types/run-output.ts"), "utf8");
    expect(new Set(tsInterfaceTopKeys(outTs, "RunOutput"))).toEqual(new Set(goTags));
  });

  // GET /runs/{id}/sign-in: RunSignIn mirrors runSignInResponse.
  it("runSignInResponse (GET /runs/{id}/sign-in): full parity with the TS RunSignIn mirror", () => {
    const goTags = goJSONTags(readFileSync(join(root, "internal/api/run_sign_in.go"), "utf8"), "runSignInResponse");
    expect(goTags.length).toBeGreaterThanOrEqual(3);
    const ts = readFileSync(join(root, "ui/src/app/lib/types/run-sign-in.ts"), "utf8");
    expect(new Set(tsInterfaceTopKeys(ts, "RunSignIn"))).toEqual(new Set(goTags));
  });

  // Key custody (key-l3.4): GET /key-domains and the key-domain fields of the credential inventory.
  it.each([
    ["internal/api/key_domains.go", "keyDomainRow", "key-domains.ts", "KeyDomainRow"],
    ["internal/api/key_domains.go", "keyDomainsResponse", "key-domains.ts", "KeyDomains"],
    ["internal/secretstore/keydomain/service.go", "Assignment", "key-domains.ts", "KeyDomainAssignment"],
    ["internal/api/credential_inventory.go", "credentialInventoryRow", "credentials.ts", "CredentialRow"],
  ])("%s %s: full parity with the TS %s mirror", (goFile, goName, tsFile, tsName) => {
    const goTags = goJSONTags(readFileSync(join(root, goFile), "utf8"), goName);
    expect(goTags.length).toBeGreaterThanOrEqual(3);
    const ts = readFileSync(join(root, "ui/src/app/lib/api", tsFile), "utf8");
    expect(new Set(tsInterfaceTopKeys(ts, tsName))).toEqual(new Set(goTags));
  });
  it.each([
    ["policyPreviewResponse", "PolicyPreviewResult"],
    ["policyPreviewSource", "PolicyPreviewSource"],
    ["policyPreviewRepository", "PolicyPreviewRepository"],
  ])("%s mirrors %s", (goName, tsName) => {
    const go = readFileSync(join(root, "internal/api/policy_preview_facts.go"), "utf8");
    const ts = readFileSync(join(root, "ui/src/app/lib/types/policy-preview.ts"), "utf8");
    expect(new Set(tsInterfaceTopKeys(ts, tsName))).toEqual(new Set(goJSONTags(go, goName)));
  });

  it("policy-preview pending reasons have the same closed enum in Go and TS", () => {
    const go = readFileSync(join(root, "internal/api/policy_preview_facts.go"), "utf8");
    const ts = readFileSync(join(root, "ui/src/app/lib/types/policy-preview.ts"), "utf8");
    const goValues = [...go.matchAll(/policyPreviewPending\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]);
    const pending = /export type PolicyPreviewPending =([\s\S]*?);/.exec(ts)?.[1] ?? "";
    const tsValues = [...pending.matchAll(/"([a-z_]+)"/g)].map((m) => m[1]);
    expect(goValues).toHaveLength(8);
    expect(new Set(tsValues)).toEqual(new Set(goValues));
  });

  // Components (#1914). The stored shapes and their SDK/server twins, then the
  // facts both dry-run doors return. ComponentSaved embeds Component in Go, so
  // it is pinned separately below, as ModelProvidersRead is.
  it.each([
    ["internal/types/component.go", "ComponentDelivery", "ComponentDelivery"],
    ["internal/types/component.go", "ComponentSecret", "ComponentSecret"],
    ["internal/types/component.go", "ComponentDefinition", "ComponentDefinition"],
    ["internal/types/component.go", "Component", "Component"],
    ["internal/types/component.go", "ComponentRef", "ComponentRef"],
    ["pkg/client/components.go", "ComponentRequest", "ComponentRequest"],
    ["pkg/client/components.go", "ComponentRequirement", "ComponentRequirement"],
    ["pkg/client/components.go", "ComponentSecretView", "ComponentSecretView"],
    ["pkg/client/components.go", "OrgComponentView", "OrgComponentView"],
    ["pkg/client/components.go", "MyComponents", "MyComponents"],
    ["internal/api/components_facts.go", "componentFact", "ComponentFact"],
    ["internal/api/components_facts.go", "componentSecretFact", "ComponentSecretFact"],
    ["internal/types/site_config.go", "ComponentSettings", "ComponentSettings", "site.ts"],
    ["internal/api/preflight.go", "preflightResponse", "PreflightResult", "runs.ts"],
    ["internal/api/permissions.go", "meCapabilitiesResponse", "MeCapabilities", "permissions.ts"],
    ["internal/api/run_revive.go", "adminRestartResult", "AdminRestartResult", "runs.ts"],
  ])("%s %s: full parity with the TS %s mirror", (goFile, goName, tsName, tsFile = "components.ts") => {
    const goTags = goJSONTags(readFileSync(join(root, goFile), "utf8"), goName);
    expect(goTags.length).toBeGreaterThanOrEqual(2); // stale-regex guard
    const ts = readFileSync(join(root, "ui/src/app/lib/types", tsFile), "utf8");
    expect(new Set(tsInterfaceTopKeys(ts, tsName))).toEqual(new Set(goTags));
  });

  // The 0.9 New Run contract (pkg/client/runs_new_run.go): the request fields and
  // the dry-run facts, each pinned to its TS mirror in types/new-run-contract.ts.
  it.each([
    ["RunResources", "RequestedResources"],
    ["ResourceAmounts", "ResourceAmounts"],
    ["PlacementResources", "PlacementResources"],
    ["RunOverrides", "RunOverrides"],
    ["AgentOverrides", "AgentOverrides"],
    ["SecretOverride", "SecretOverride"],
    ["ADOOverrides", "ADOOverrides"],
    ["GitPATOverride", "GitPATOverride"],
    ["PushRuleOverride", "PushRuleOverride"],
    ["LocalPlacementFact", "LocalPlacementFact"],
    ["TokenScopeFact", "TokenScopeFact"],
    ["RepoAccessFact", "RepoAccessFact"],
    ["AgentModelProviderFact", "AgentModelProviderFact"],
    ["AgentHostFact", "AgentHostFact"],
    ["AgentSecretFact", "AgentSecretFact"],
    ["ManagedSettingsFact", "ManagedSettingsFact"],
    ["TelemetryFact", "TelemetryFact"],
    ["AgentFact", "AgentFact"],
  ])("pkg/client %s: full parity with the TS %s mirror", (goName, tsName) => {
    const goTags = goJSONTags(readFileSync(join(root, "pkg/client/runs_new_run.go"), "utf8"), goName);
    expect(goTags.length).toBeGreaterThanOrEqual(1);
    const ts = readFileSync(join(root, "ui/src/app/lib/types/new-run-contract.ts"), "utf8");
    expect(new Set(tsInterfaceTopKeys(ts, tsName))).toEqual(new Set(goTags));
  });

  it("the New Run contract's closed value sets match Go", () => {
    const ts = stripComments(readFileSync(join(root, "ui/src/app/lib/types/new-run-contract.ts"), "utf8"));
    const union = (name: string) => {
      const m = new RegExp(`export type ${name} =([^;]+);`).exec(ts);
      if (!m) throw new Error(`type ${name} not found`);
      return new Set([...m[1].matchAll(/"([a-z_]+)"/g)].map((x) => x[1]));
    };
    const placementGo = readFileSync(join(root, "internal/placement/placement.go"), "utf8");
    const deliveryGo = readFileSync(join(root, "internal/placement/delivery.go"), "utf8");
    const clientGo = readFileSync(join(root, "pkg/client/runs_new_run.go"), "utf8");
    expect(union("PlacementValue")).toEqual(new Set([...placementGo.matchAll(/\b(?:Remote|Local)\s+Placement = "([a-z_]+)"/g)].map((m) => m[1])));
    expect(union("LocalDeliveryMode")).toEqual(new Set([...deliveryGo.matchAll(/\bMode\w+\s+Mode = "([a-z_]+)"/g)].map((m) => m[1])));
    expect(union("LocalPlacementKind")).toEqual(new Set([...clientGo.matchAll(/\bLocalPlacement(?:Host|Source|Component)\s*=\s*"([a-z_]+)"/g)].map((m) => m[1])));
  });

  it("ComponentSaved (POST/PUT /me/components, PUT /components/{id}): Component plus requirements", () => {
    const go = readFileSync(join(root, "pkg/client/components.go"), "utf8");
    expect(go).toMatch(/type ComponentSaved struct \{\n\tComponent\n/);
    const ts = readFileSync(join(root, "ui/src/app/lib/types/components.ts"), "utf8");
    expect(ts).toMatch(/export interface ComponentSaved extends Component \{/);
    expect(new Set(tsInterfaceTopKeys(ts, "ComponentSaved"))).toEqual(new Set(goJSONTags(go, "ComponentSaved")));
  });

  it("PolicySaved (POST /policies, PUT /policies/{id}): RunPolicy plus the component requirement rows", () => {
    const go = readFileSync(join(root, "pkg/client/policies.go"), "utf8");
    expect(go).toMatch(/type PolicySaved struct \{\n\ttypes\.RunPolicy\n\tRequirements \[\]ComponentRequirement `json:"requirements"`\n\}/);
    const ts = readFileSync(join(root, "ui/src/app/lib/api/policies.ts"), "utf8");
    expect(ts).toMatch(/export type SaveRequirement = ComponentRequirement;/);
    expect(ts).toMatch(/export type PolicySaved = RunPolicy & \{ requirements: SaveRequirement\[\] \};/);
  });

  it("component closed value sets (delivery modes, fact status, reason, lane, kind) match Go", () => {
    const ts = readFileSync(join(root, "ui/src/app/lib/types/components.ts"), "utf8");
    const typeGo = readFileSync(join(root, "internal/types/component.go"), "utf8");
    const factsGo = readFileSync(join(root, "internal/api/components_facts.go"), "utf8");
    const runGo = readFileSync(join(root, "internal/api/components_run.go"), "utf8");
    const providerGo = readFileSync(join(root, "internal/types/workspace_provider.go"), "utf8");
    const union = (name: string) => {
      const m = new RegExp(`export type ${name} =([^;]+);`).exec(stripComments(ts));
      if (!m) throw new Error(`type ${name} not found`);
      return new Set([...m[1].matchAll(/"([a-z_]+)"/g)].map((x) => x[1]));
    };
    // Open-ended on purpose: every const of the Go type, whatever its name, so an
    // ADDED value fails until the TS union names it (a name list would only catch renames).
    const consts = (src: string, pattern: string) => new Set([...src.matchAll(new RegExp(pattern, "g"))].map((m) => m[1]));
    expect(union("ComponentDeliveryMode")).toEqual(consts(typeGo, '\\bComponentDelivery\\w+\\s*=\\s*"([a-z_]+)"'));
    const statusSrc = /What a fact's status says\.\s*\nconst \(([\s\S]*?)\n\)/.exec(factsGo)?.[1];
    expect(statusSrc, "the fact status const block moved").toBeDefined();
    expect(union("ComponentFactStatus")).toEqual(consts(statusSrc!, '\\bcomponent\\w+\\s*=\\s*"([a-z_]+)"'));
    expect(union("ComponentFactStatus").size).toBeGreaterThanOrEqual(4);
    // org, self and inline are the gate's; "workspace" is the Git provider row's literal.
    expect(union("ComponentFactReason")).toEqual(new Set([...consts(runGo, '\\bcomponentSource\\w+\\s*=\\s*"([a-z_]+)"'), "workspace"]));
    expect(factsGo).toMatch(/Reason: "workspace"/);
    expect(union("ComponentFactLane")).toEqual(
      new Set([...consts(providerGo, '\\bGitLane\\w+\\s+GitLane = "([a-z_]+)"'), ...consts(factsGo, '\\bgitLane\\w+\\s*=\\s*"([a-z_]+)"')]),
    );
    expect(union("ComponentFactKind")).toEqual(
      new Set([...typeGo.matchAll(/Component(?:Custom|GitProvider|Agent|GitPAT)\s+ComponentKind = "([a-z_]+)"/g)].map((m) => m[1])),
    );
  });

  // Every Go GrantKind, so an added kind fails until policy.ts names it. policy-tab-copy.ts's
  // display labels are copy and are not pinned here.
  it("GrantKind: the TS union is exactly the Go GrantKind consts", () => {
    const go = readFileSync(join(root, "internal/types/types.go"), "utf8");
    const policyTs = readFileSync(join(root, "ui/src/app/lib/types/policy.ts"), "utf8");
    const goKinds = new Set([...go.matchAll(/\bGrant\w+\s+GrantKind = "([a-z_]+)"/g)].map((m) => m[1]));
    expect(goKinds.size).toBeGreaterThanOrEqual(7);
    const m = /export type GrantKind =([^;]+);/.exec(stripComments(policyTs));
    if (!m) throw new Error("type GrantKind not found");
    expect(new Set([...m[1].matchAll(/"([a-z_]+)"/g)].map((x) => x[1]))).toEqual(goKinds);
  });

  // AutonomyResolution.bound_by names a rubric row or the org's component cap.
  it("AutonomyBoundKey names the Go component-cap cause beside the rubric rows", () => {
    const go = readFileSync(join(root, "internal/api/runs_autonomy_components.go"), "utf8");
    const cause = /\bcomponentAutonomyCause\s*=\s*"([a-z_]+)"/.exec(go)?.[1];
    expect(cause).toBe("custom_component");
    const govTs = readFileSync(join(root, "ui/src/app/lib/api/governance.ts"), "utf8");
    const m = /export type AutonomyBoundKey =([^;]+);/.exec(stripComments(govTs));
    expect(m?.[1]).toContain(`"${cause}"`);
    expect(tsInterfaceBody(govTs, "AutonomyResolution")).toMatch(/bound_by\?: AutonomyBoundKey\[\];/);
  });

  it("SetupItemResidency names every residency the setup checklist emits", () => {
    const goRaw = readFileSync(join(root, "internal/api/compose_setup.go"), "utf8");
    const tsRuns = readFileSync(join(root, "ui/src/app/lib/types/runs.ts"), "utf8");
    const union = /export type SetupItemResidency =([^;]+);/.exec(stripComments(tsRuns))?.[1] ?? "";
    const tsValues = new Set([...union.matchAll(/"([a-z_]+)"/g)].map((m) => m[1]));
    // Code only: whole-line comments (the field's doc block quotes every value) are dropped,
    // so a value counts only when something assigns it.
    const go = goRaw.replace(/^\s*\/\/.*$/gm, "");
    const assigned = new Set<string>();
    for (const m of go.matchAll(/\bResidency\s*[:=]\s*"([a-z_]+)"(?!\s*,\s*")/g)) assigned.add(m[1]);
    // `it.Status, it.Residency = "satisfied", "none"`
    for (const m of go.matchAll(/\bResidency\s*=\s*"[a-z_]+",\s*"([a-z_]+)"/g)) assigned.add(m[1]);
    // add(secret, label, residency) in setupSecretItems
    for (const m of go.matchAll(/\badd\([^\n]*,\s*"([a-z_]+)"\)/g)) assigned.add(m[1]);
    expect(assigned.size).toBeGreaterThanOrEqual(6);
    expect(tsValues).toEqual(assigned);
  });

});
