/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// T-69 — Go->TS wire parity for the DTOs the F8 console-e2e audit named that
// had no source-parity probe yet: SetupStatus, SetupModelAccess (the
// `model_access` chip state), ApprovalRequest, and the attach-mode control
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
  const bedrockGo = readFileSync(join(root, "internal/api/runs_bedrock_probe.go"), "utf8");
  const harnessToolGo = readFileSync(join(root, "internal/api/setup_integrations.go"), "utf8");
  const modelAccessGo = readFileSync(join(root, "internal/api/modelaccess.go"), "utf8");
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
    ["SetupHarness", setupGo],
    ["SetupBedrock", bedrockGo],
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
    ["SetupDeployment", "deployment"],
  ])("%s: full parity with SetupStatus.%s's inline TS type", (goName, member) => {
    const goTags = goJSONTags(setupGo, goName);
    expect(goTags.length).toBeGreaterThan(0);
    expect(new Set(tsInlineKeys(setupTs, "SetupStatus", member))).toEqual(new Set(goTags));
  });

  it("SetupModelAccess (`model_access`): full parity with the TS mirror", () => {
    const goTags = goJSONTags(modelAccessGo, "SetupModelAccess");
    expect(goTags.length).toBeGreaterThanOrEqual(4);
    const tsKeys = tsInterfaceTopKeys(setupTs, "SetupModelAccess");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
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
});
