/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared test fixtures for the two bodies nearly every screen suite has to
// stand up: GET /setup/status and GET /me. baseStatus lived under
// screens/setup/ until that funnel was deleted; 18 suites depend on it, most of
// them nothing to do with setup, so it belongs in lib/ — and the /me pair
// followed it here for the same reason, once seven suites had retyped the same
// member body and four had retyped the same drive.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, it } from "vitest";
import type { Me, MeUserDrive } from "./api/health";
import type { SetupModelProvider, SetupProviderAccess, SetupStatus } from "./types";

// The multi-provider design's fixture providers (§5), as /setup/status
// publishes them to one person — shared by the door, the strip and the
// entrances that open it.
export const MODEL_PROVIDERS = {
  bedrock: {
    id: "bedrock-prod",
    name: "Bedrock (prod)",
    kind: "bedrock_sso",
    harnesses: ["claude-code"],
    host: "bedrock-runtime.us-east-1.amazonaws.com",
  },
  claude: {
    id: "claude-sub",
    name: "Claude subscription",
    kind: "anthropic_subscription",
    harnesses: ["claude-code"],
    host: "api.anthropic.com",
  },
  anthropicKey: {
    id: "anthropic-key",
    name: "Anthropic API key",
    kind: "anthropic_api_key",
    harnesses: ["claude-code"],
    host: "api.anthropic.com",
  },
  gateway: {
    id: "corp-gateway",
    name: "Corp gateway",
    kind: "custom_endpoint",
    harnesses: ["claude-code", "codex-cli"],
    host: "gateway.corp.example",
  },
} satisfies Record<string, SetupModelProvider>;

/** A status carrying a provider block: each provider with the agents it is the
 *  default for, and this person's state for it (not_configured when omitted). */
export function providerStatus(
  rows: { provider: SetupModelProvider; defaultFor?: string[]; state?: string; deadline?: string }[],
  overrides: Partial<SetupStatus> = {},
): SetupStatus {
  return baseStatus({
    model_providers: rows.map((r) => ({ ...r.provider, default_for: r.defaultFor })),
    provider_access: rows.map(
      (r): SetupProviderAccess => ({ provider: r.provider.id, state: r.state ?? "not_configured", deadline: r.deadline }),
    ),
    ...overrides,
  });
}

export function baseStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return {
    ready: false,
    checks: [],
    auth: { mode: "local", local_loopback: true },
    runner: {
      driver: "docker",
      confinement_classes: ["CC1", "CC2"],
      confinement_substrates: { CC1: "oci/runc", CC2: "oci/runsc" },
    },
    providers: [{ tool: "claude", installed: true, logged_in: false }],
    secrets: { present: [], github_app: false },
    age_key: { durable: false },
    has_runs: false,
    platform: { os: "linux", wsl: false, kvm: true },
    ...overrides,
  };
}

// GET /me for a plain MEMBER — the tier every drive/onboarding suite is
// written from, with NEITHER 0.7 drive bit set: no allocation and no door,
// which is what a deployment that has never registered a drive answers and
// what every case that isn't about drives must render as.
export function baseMe(overrides: Partial<Me> = {}): Me {
  return {
    principal: "alice@corp.example",
    method: "sso",
    operator: false,
    security_operator: false,
    role: "user",
    email: "alice@corp.example",
    user_drive: null,
    user_drive_denied_by_profile: "",
    ...overrides,
  };
}

// /me's `user_drive` — a managed claim with a size that IS shown and a
// writable mount, the case both member surfaces render most of. The share
// (no size, read-only, `external`) and the paused arms are overrides of it.
export function baseMeDrive(overrides: Partial<MeUserDrive> = {}): MeUserDrive {
  return {
    name: "Scratch",
    backend: "k8s_pvc",
    size_mib: 16384,
    writable: true,
    enforcement: "request",
    ...overrides,
  };
}

// The canon pin, shared

// expectNoOwnCopy is THE canon pin two screen directories both need: a
// component in a copy-governed directory may RENDER copy, never author it, so a
// quoted or bare prose string in its source is a canon break rather than a
// style question. Declared once here because the scan was byte-for-byte
// identical in both suites and only DIR/FILES differed — a second copy is a
// second thing to keep in step with the strip rules.
//
// Call it inside the owning suite so the failure still names that directory.
export function expectNoOwnCopy(dir: string, files: string[]) {
  const source = (f: string) => readFileSync(join(process.cwd(), dir, f), "utf8");

  // Comments explain, they do not render; className, cn() class lists and
  // data-testid are machinery. Everything left is a candidate for the eye.
  const strip = (src: string) =>
    src
      .replace(/\/\*[\s\S]*?\*\//g, "")
      .replace(/^\s*\/\/.*$/gm, "")
      .replace(/\{\/\*[\s\S]*?\*\/\}/g, "")
      .replace(/className=(\{[^{}]*\}|"[^"]*")/g, "")
      .replace(/\bcn\([^()]*\)/g, "")
      .replace(/data-testid="[^"]*"/g, "");

  // Two words of prose is the signal: import paths, wire keys and CSS tokens
  // are single words or hyphenated, and a rendered sentence is not.
  const PROSE = /[A-Za-z]{2,}\s+[A-Za-z]{2,}/;

  it.each(files)("%s holds no quoted product prose", (f) => {
    const quoted = [...strip(source(f)).matchAll(/"([^"\n]*)"|'([^'\n]*)'/g)]
      .map((m) => m[1] ?? m[2])
      .filter((s) => PROSE.test(s));
    expect(quoted).toEqual([]);
  });

  it.each(files)("%s holds no bare JSX text node either", (f) => {
    const bare = [...strip(source(f)).matchAll(/>([^<>{}\n]{4,})</g)]
      .map((m) => m[1].trim())
      .filter((s) => PROSE.test(s));
    expect(bare).toEqual([]);
  });
}
