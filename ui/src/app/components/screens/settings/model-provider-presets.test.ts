/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A gateway preset fills the fields it names and nothing else. Pure, so every fill is asserted on the draft.
import { describe, it, expect } from "vitest";
import { PROVIDER_EDITOR as E } from "../../../lib/model-providers-copy";
import { newDraft, type ProviderDraft } from "./model-provider-draft";
import { applyPreset, type PresetId } from "./model-provider-presets";

const HARNESSES = [
  { id: "claude-code", display: "Claude Code" },
  { id: "codex-cli", display: "Codex CLI" },
];
const fresh = (): ProviderDraft => newDraft("custom_endpoint", HARNESSES);
const harness = (d: ProviderDraft, id: string) => d.harnesses[id];

// Every field a draft has, minus the ones a preset is allowed to name.
const rest = (d: ProviderDraft) => {
  const { harnesses, headers, authHeader, authFormat, baseUrl, ...others } = d;
  void harnesses, headers, authHeader, authFormat, baseUrl;
  return others;
};

describe("applyPreset: what each preset fills", () => {
  it("LiteLLM: Claude Code path / and Codex CLI path /v1", () => {
    const base = fresh();
    const out = applyPreset(base, "litellm");
    expect(out).toEqual({
      ...base,
      harnesses: {
        "claude-code": { ...harness(base, "claude-code"), path: "/" },
        "codex-cli": { ...harness(base, "codex-cli"), path: "/v1" },
      },
    });
  });

  it("Portkey self-hosted: Claude Code ticked with path /, Codex CLI unticked, header x-portkey-provider: anthropic", () => {
    const base = fresh();
    const out = applyPreset(base, "portkey_self_hosted");
    expect(out).toEqual({
      ...base,
      headers: [{ name: "x-portkey-provider", value: "anthropic" }],
      harnesses: {
        "claude-code": { ...harness(base, "claude-code"), ticked: true, path: "/" },
        "codex-cli": { ...harness(base, "codex-cli"), ticked: false },
      },
    });
  });

  it("Portkey hosted: x-portkey-api-key with {token}, and an empty x-portkey-provider row with its placeholder; no base URL or path", () => {
    const base = fresh();
    const out = applyPreset(base, "portkey_hosted");
    expect(out).toEqual({
      ...base,
      authHeader: "x-portkey-api-key",
      authFormat: "{token}",
      headers: [{ name: "x-portkey-provider", value: "", placeholder: E.PORTKEY_SLUG_PLACEHOLDER }],
    });
    expect(out.baseUrl).toBe("");
    expect(harness(out, "claude-code").path).toBe("");
    expect(harness(out, "codex-cli").path).toBe("");
  });

  it("OpenRouter: base URL, Claude Code path /, Codex CLI path /v1", () => {
    const base = fresh();
    const out = applyPreset(base, "openrouter");
    expect(out).toEqual({
      ...base,
      baseUrl: "https://openrouter.ai/api",
      harnesses: {
        "claude-code": { ...harness(base, "claude-code"), path: "/" },
        "codex-cli": { ...harness(base, "codex-cli"), path: "/v1" },
      },
    });
  });

  it("Other changes nothing", () => {
    const base = { ...fresh(), name: "Mine", baseUrl: "https://gw.example", headers: [{ name: "x-a", value: "b" }] };
    expect(applyPreset(base, "other")).toEqual(base);
  });

  it("returns a new draft and leaves the one it was given as it was", () => {
    const base = fresh();
    const before = JSON.stringify(base);
    const out = applyPreset(base, "portkey_self_hosted");
    expect(out).not.toBe(base);
    expect(JSON.stringify(base)).toBe(before);
  });
});

describe("applyPreset: fields it does not name are untouched", () => {
  const dirty = (): ProviderDraft => ({
    ...fresh(),
    name: "Corp gateway",
    gatewayOn: true,
    gatewayAudience: "llm",
    oidcOn: true,
    oidcIssuer: "https://sso.example.com",
    harnesses: {
      "claude-code": { ticked: true, model: "my-model", path: "" },
      "codex-cli": { ticked: true, model: "codex-model", path: "" },
    },
  });

  it.each<PresetId>(["litellm", "portkey_self_hosted", "portkey_hosted", "openrouter", "other"])("%s keeps the name, models and every other field", (id) => {
    const base = dirty();
    const out = applyPreset(base, id);
    expect(rest(out)).toEqual(rest(base));
    expect(harness(out, "claude-code").model).toBe("my-model");
    expect(harness(out, "codex-cli").model).toBe("codex-model");
  });

  it.each<PresetId>(["litellm", "portkey_self_hosted", "openrouter", "other"])("%s leaves the auth header and value format as typed", (id) => {
    const base = { ...dirty(), authHeader: "X-Api-Key", authFormat: "{token}" };
    const out = applyPreset(base, id);
    expect(out.authHeader).toBe("X-Api-Key");
    expect(out.authFormat).toBe("{token}");
  });

  it.each<PresetId>(["litellm", "portkey_self_hosted", "portkey_hosted", "other"])("%s leaves a base URL as typed", (id) => {
    expect(applyPreset({ ...dirty(), baseUrl: "https://gw.example" }, id).baseUrl).toBe("https://gw.example");
  });
});

describe("applyPreset: earlier fills stay", () => {
  it("Portkey self-hosted then LiteLLM keeps the x-portkey-provider row and Codex CLI unticked", () => {
    const out = applyPreset(applyPreset(fresh(), "portkey_self_hosted"), "litellm");
    expect(out.headers).toEqual([{ name: "x-portkey-provider", value: "anthropic" }]);
    expect(harness(out, "codex-cli").ticked).toBe(false);
    expect(harness(out, "codex-cli").path).toBe("/v1");
    expect(harness(out, "claude-code").path).toBe("/");
  });

  it("pressing Portkey self-hosted twice leaves one row", () => {
    const out = applyPreset(applyPreset(fresh(), "portkey_self_hosted"), "portkey_self_hosted");
    expect(out.headers).toEqual([{ name: "x-portkey-provider", value: "anthropic" }]);
  });

  it("a row of the same name in another case is replaced, never duplicated; the rows beside it stay", () => {
    const base = { ...fresh(), headers: [{ name: "X-Team", value: "a" }, { name: "X-Portkey-Provider", value: "old" }] };
    const out = applyPreset(base, "portkey_hosted");
    expect(out.headers).toEqual([
      { name: "X-Team", value: "a" },
      { name: "x-portkey-provider", value: "", placeholder: E.PORTKEY_SLUG_PLACEHOLDER },
    ]);
  });
});
