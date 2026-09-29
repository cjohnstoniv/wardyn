/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { SetupStatus } from "./types";
import { hasLlmPath, deriveReadiness, deploymentMode } from "./readiness";
import { baseStatus } from "./test-fixtures";

// A minimal-but-valid SetupStatus: no CLI login and no key secret — the case
// that must NOT read as real LLM access. This suite's own pins: ready, CC1-only
// runner, a non-durable-loopback local auth, no providers, and a durable secret
// store.
function status(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return baseStatus({
    ready: true,
    runner: { driver: "docker", confinement_classes: ["CC1"] },
    auth: { mode: "local", local_loopback: false },
    providers: [],
    age_key: { durable: true },
    platform: { os: "linux", wsl: false },
    ...overrides,
  });
}

// llmReady is the server's own llm_ready (an enabled model provider serves a
// harness) and llmLabel names that provider. Since 0.8 (#548) a run's model
// credential comes only from its model provider, so an operator key, a host
// CLI login or Bedrock boot config is no LLM path at all.
describe("hasLlmPath — the server's llm_ready", () => {
  it("a bare status is no LLM access", () => {
    expect(hasLlmPath(status())).toBe(false);
  });

  it("an enabled provider (llm_ready) is LLM access, redacted view or not", () => {
    expect(hasLlmPath(status({ llm_ready: true }))).toBe(true);
    expect(hasLlmPath(status({ checks_redacted: true, llm_ready: true }))).toBe(true);
  });

  it("an operator key, a host CLI login or Bedrock boot config is no LLM path", () => {
    expect(hasLlmPath(status({ secrets: { present: ["anthropic-api-key", "openai-api-key"], github_app: false } }))).toBe(false);
    expect(hasLlmPath(status({ providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }] }))).toBe(false);
    expect(hasLlmPath(status({ bedrock: { region: "us-east-1", model: "anthropic.claude-3", creds_present: true } }))).toBe(false);
  });
});

describe("deriveReadiness — must not overclaim a connected model", () => {
  it("nothing configured: llmReady/composerReady false and no label", () => {
    const r = deriveReadiness(status());
    expect(r.llmReady).toBe(false);
    expect(r.llmLabel).toBe("");
    expect(r.composerReady).toBe(false);
  });

  it("llm_ready names the first enabled provider serving a harness", () => {
    const r = deriveReadiness(
      status({
        llm_ready: true,
        model_providers: [
          { id: "off", kind: "anthropic_api_key", disabled: true, harnesses: ["claude-code"], host: "api.anthropic.com" },
          { id: "corp", name: "Corp gateway", kind: "custom_endpoint", harnesses: ["claude-code"], host: "gw.corp.example" },
        ],
      }),
    );
    expect(r.llmReady).toBe(true);
    expect(r.llmLabel).toBe("Corp gateway");
  });

  it("a redacted view that lists no provider still reads llmReady, with no label to offer", () => {
    const r = deriveReadiness(status({ checks_redacted: true, llm_ready: true }));
    expect(r.llmReady).toBe(true);
    expect(r.llmLabel).toBe("");
  });

  it("an operator Anthropic key still powers Wardyn features, but is no agent path", () => {
    const r = deriveReadiness(status({ secrets: { present: ["anthropic-api-key"], github_app: false } }));
    expect(r.llmReady).toBe(false);
    expect(r.composerReady).toBe(true);
  });

  // Azure powered Wardyn's own features and no agent tool; those features (the
  // AI Run Composer) are deleted, so its stored secret is inert.
  it("a stored Azure key is inert — neither an agent path nor a Wardyn-features path", () => {
    const r = deriveReadiness(status({ secrets: { present: ["azure-openai-key"], github_app: false } }));
    expect(r.llmReady).toBe(false);
    expect(r.composerReady).toBe(false);
  });
});

// deploymentMode — only `sso` widens to multi-user; every other auth.mode,
// including a future one this UI has never heard of, reads single-user.
describe("deploymentMode", () => {
  it("local reads single-user", () => {
    expect(deploymentMode(status({ auth: { mode: "local", local_loopback: false } }))).toBe("single-user");
  });

  it("token reads single-user", () => {
    expect(deploymentMode(status({ auth: { mode: "token", local_loopback: false } }))).toBe("single-user");
  });

  it("disabled reads single-user", () => {
    expect(deploymentMode(status({ auth: { mode: "disabled", local_loopback: false } }))).toBe("single-user");
  });

  it("sso reads multi-user", () => {
    expect(deploymentMode(status({ auth: { mode: "sso", local_loopback: false } }))).toBe("multi-user");
  });

  // Negative control: an unknown/future mode string must never widen to
  // multi-user — single-user is the narrower, safer default.
  it("an unknown mode reads single-user, not multi-user", () => {
    expect(
      deploymentMode(status({ auth: { mode: "future-mode" as SetupStatus["auth"]["mode"], local_loopback: false } })),
    ).toBe("single-user");
  });
});

