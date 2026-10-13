/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Wire parity for the #1508 mirrors: each TS interface of types/service-identities.ts has exactly the json
// tags of the Go struct it mirrors, and each closed value set is exactly the Go constants. The Go side is
// read from source.

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

const root = repoRoot();
const read = (p: string) => readFileSync(join(root, p), "utf8");

function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  return [...m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((t) => t[1]).filter((t) => t !== "-");
}

const stripComments = (src: string) => src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");

function tsKeys(src: string, iface: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${iface}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${iface} not found`);
  return [...m[1].matchAll(/^\s*([a-z0-9_]+)\??\s*:/gm)].map((k) => k[1]);
}

function tsUnion(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+type\\s+${name}\\s*=([^;]+);`).exec(stripComments(src));
  if (!m) throw new Error(`type ${name} not found`);
  return [...m[1].matchAll(/"([a-z_]+)"/g)].map((x) => x[1]);
}

function goConsts(src: string, type: string): string[] {
  return [...src.matchAll(new RegExp(`\\w+\\s+${type}\\s*=\\s*"([a-z_]+)"`, "g"))].map((m) => m[1]);
}

const ts = read("ui/src/app/lib/types/service-identities.ts");

describe("invitations and service identities wire", () => {
  it.each([
    ["internal/types/invitation.go", "Invitation", "Invitation"],
    ["internal/types/service_identity.go", "ServiceIdentity", "ServiceIdentity"],
    ["internal/types/types.go", "APIToken", "ApiToken"],
    ["pkg/client/invitations.go", "ClaimInvitationRequest", "ClaimInvitationRequest"],
    ["pkg/client/service_identities.go", "MintServiceTokenRequest", "MintServiceTokenRequest"],
  ])("%#: %s has full parity with its TS mirror", (file, goName, tsName) => {
    const tags = goJSONTags(read(file), goName);
    expect(tags.length).toBeGreaterThanOrEqual(2);
    expect(new Set(tsKeys(ts, tsName))).toEqual(new Set(tags));
  });

  it.each([
    ["InvitationState", "internal/types/invitation.go", "InvitationState"],
    ["ServiceIdentityState", "internal/types/service_identity.go", "ServiceIdentityState"],
    ["TokenDisposition", "internal/types/token_move.go", "TokenDisposition"],
  ])("%s is the Go constants", (tsName, file, goType) => {
    const go = goConsts(read(file), goType);
    expect(go.length).toBeGreaterThanOrEqual(4);
    expect(new Set(tsUnion(ts, tsName))).toEqual(new Set(go));
  });
});
