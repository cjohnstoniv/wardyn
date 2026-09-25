/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #542 — pure derivations behind the New Run rail's provider picker: the
// candidate set (R1's sole survivor, R2/R3/R6/R7/R8's list), each option's
// label (QC-2), and resolveProviderSelection's R2/R6/R7/R8 rule. Canon-pinned
// against RAIL_PROVIDER so a copy edit here fails loudly rather than drifting.
import { describe, expect, it } from "vitest";
import {
  accessStateFor,
  defaultCandidate,
  providerCandidates,
  providerConnected,
  providerGate,
  providerOptionLabel,
  providerResidency,
  providerStateWord,
  providerWhatWord,
  resolveProviderSelection,
} from "./model-provider-lane";
import { RAIL_PROVIDER } from "../../wardyn/copy/new-run-rail";
import { MODEL_PROVIDERS } from "../../../lib/test-fixtures";

const { bedrock, claude, anthropicKey, gateway } = MODEL_PROVIDERS;

describe("providerCandidates", () => {
  it("keeps only providers that serve the agent and are not disabled", () => {
    const providers = [bedrock, { ...gateway, disabled: true }, { ...claude, harnesses: ["codex-cli"] }];
    expect(providerCandidates(providers, "claude-code")).toEqual([bedrock]);
  });

  it("is empty with no providers at all (R9's shape)", () => {
    expect(providerCandidates(undefined, "claude-code")).toEqual([]);
  });
});

describe("accessStateFor / providerConnected", () => {
  it("reads not_configured for a provider with no access row", () => {
    expect(accessStateFor([], "bedrock-prod")).toBe("not_configured");
  });

  it("live and expiring both read as connected; everything else does not", () => {
    expect(providerConnected("live")).toBe(true);
    expect(providerConnected("expiring")).toBe(true);
    expect(providerConnected("expired_signin")).toBe(false);
    expect(providerConnected("not_configured")).toBe(false);
  });
});

describe("providerWhatWord / providerStateWord / providerOptionLabel — QC-2", () => {
  it.each([
    ["bedrock_sso", "AWS sign-in"],
    ["anthropic_subscription", "sign-in"],
    ["custom_endpoint", "token"],
    ["anthropic_api_key", "key"],
    ["openai_api_key", "key"],
  ])("%s reads as %s", (kind, word) => {
    expect(providerWhatWord(kind)).toBe(word);
  });

  it("a sign-in kind reads signed in / not signed in", () => {
    expect(providerStateWord("bedrock_sso", "live")).toBe("signed in");
    expect(providerStateWord("bedrock_sso", "not_configured")).toBe("not signed in");
  });

  it("a key/token kind reads added / not added", () => {
    expect(providerStateWord("custom_endpoint", "live")).toBe("added");
    expect(providerStateWord("anthropic_api_key", "not_configured")).toBe("not added");
  });

  it("renders RAIL_PROVIDER.OPTION verbatim — packet C's own examples", () => {
    const access = [
      { provider: gateway.id, state: "live" },
      { provider: anthropicKey.id, state: "not_configured" },
      { provider: claude.id, state: "live" },
      { provider: bedrock.id, state: "not_configured" },
    ];
    expect(providerOptionLabel(gateway, access)).toBe(RAIL_PROVIDER.OPTION("Corp gateway", "token", "added"));
    expect(providerOptionLabel(anthropicKey, access)).toBe(
      RAIL_PROVIDER.OPTION("Anthropic API key", "key", "not added"),
    );
    expect(providerOptionLabel(claude, access)).toBe(RAIL_PROVIDER.OPTION("Claude subscription", "sign-in", "signed in"));
    expect(providerOptionLabel(bedrock, access)).toBe(
      RAIL_PROVIDER.OPTION("Bedrock (prod)", "AWS sign-in", "not signed in"),
    );
  });
});

describe("providerResidency — R4's grouping", () => {
  it("bedrock_sso is sandbox; every other kind is proxy", () => {
    expect(providerResidency("bedrock_sso")).toBe("sandbox");
    expect(providerResidency("custom_endpoint")).toBe("proxy");
    expect(providerResidency("anthropic_api_key")).toBe("proxy");
    expect(providerResidency("openai_api_key")).toBe("proxy");
    expect(providerResidency("anthropic_subscription")).toBe("proxy");
  });
});

describe("defaultCandidate", () => {
  it("finds the candidate whose default_for names the agent", () => {
    const candidates = [gateway, { ...anthropicKey, default_for: ["claude-code"] }];
    expect(defaultCandidate(candidates, "claude-code")?.id).toBe(anthropicKey.id);
  });

  it("is undefined with no default among the candidates (R6)", () => {
    expect(defaultCandidate([gateway, anthropicKey], "claude-code")).toBeUndefined();
  });
});

describe("resolveProviderSelection — R2/R6/R7/R8", () => {
  const CLAUDE_CODE = "claude-code";
  const CODEX = "codex-cli";

  it("R2: preselects the agent's default among several candidates, silently", () => {
    const candidates = [gateway, { ...anthropicKey, default_for: [CLAUDE_CODE] }];
    const result = resolveProviderSelection({
      candidates,
      agent: CLAUDE_CODE,
      agentLabel: "Claude Code",
      previousId: undefined,
      previousName: undefined,
      agentChanged: false,
    });
    expect(result).toEqual({ selectedId: anthropicKey.id, changeNote: null });
  });

  it("R1 (via the same rule): the sole candidate is adopted with no note", () => {
    const result = resolveProviderSelection({
      candidates: [bedrock],
      agent: CLAUDE_CODE,
      agentLabel: "Claude Code",
      previousId: undefined,
      previousName: undefined,
      agentChanged: false,
    });
    expect(result).toEqual({ selectedId: bedrock.id, changeNote: null });
  });

  it("R6 (QC-4): several candidates, no default among them — no preselection, no note", () => {
    const result = resolveProviderSelection({
      candidates: [gateway, anthropicKey],
      agent: CLAUDE_CODE,
      agentLabel: "Claude Code",
      previousId: undefined,
      previousName: undefined,
      agentChanged: false,
    });
    expect(result).toEqual({ selectedId: undefined, changeNote: null });
  });

  it("R8: switching agent keeps an explicit selection that still serves it, silently", () => {
    const candidates = [gateway]; // Corp gateway serves both claude-code and codex-cli
    const result = resolveProviderSelection({
      candidates,
      agent: CODEX,
      agentLabel: "Codex CLI",
      previousId: gateway.id,
      previousName: gateway.name,
      agentChanged: true,
    });
    expect(result).toEqual({ selectedId: gateway.id, changeNote: null });
  });

  it("R7: switching agent invalidates the selection — adopts the new default and names the change", () => {
    const openaiKey = { id: "openai-key", name: "OpenAI API key", kind: "openai_api_key", harnesses: [CODEX], host: "api.openai.com" };
    const result = resolveProviderSelection({
      candidates: [openaiKey],
      agent: CODEX,
      agentLabel: "Codex CLI",
      previousId: anthropicKey.id,
      previousName: anthropicKey.name,
      agentChanged: true,
    });
    expect(result).toEqual({
      selectedId: openaiKey.id,
      changeNote: RAIL_PROVIDER.CHANGED("OpenAI API key", "Anthropic API key", "Codex CLI"),
    });
  });

  it("never names a change on first arrival, even with no prior selection", () => {
    const result = resolveProviderSelection({
      candidates: [bedrock],
      agent: CLAUDE_CODE,
      agentLabel: "Claude Code",
      previousId: undefined,
      previousName: undefined,
      agentChanged: true, // the initial mount's ref-equal guard is the caller's job; this only checks the note rule
    });
    expect(result.changeNote).toBeNull();
  });

  // F2 (#612) — the workspace pin: checked ahead of the roster default,
  // silently, and never papered over by a fallback substitution.
  it("pin beats default", () => {
    const candidates = [gateway, { ...anthropicKey, default_for: [CLAUDE_CODE] }];
    const result = resolveProviderSelection({
      candidates,
      agent: CLAUDE_CODE,
      agentLabel: "Claude Code",
      previousId: undefined,
      previousName: undefined,
      agentChanged: false,
      pin: gateway.id,
    });
    expect(result).toEqual({ selectedId: gateway.id, changeNote: null });
  });

  it("pin not a candidate → undefined", () => {
    const candidates = [gateway, { ...anthropicKey, default_for: [CLAUDE_CODE] }];
    const result = resolveProviderSelection({
      candidates,
      agent: CLAUDE_CODE,
      agentLabel: "Claude Code",
      previousId: undefined,
      previousName: undefined,
      agentChanged: false,
      pin: bedrock.id, // bedrock doesn't serve this agent's candidate set here
    });
    expect(result).toEqual({ selectedId: undefined, changeNote: null });
  });

  // Opus review round 2 (F2) — the ORIGINAL bug: keeping any still-serving
  // previousId unconditionally, before ever checking the pin, let an
  // AUTOMATIC pick (this function's own earlier adoption) outlive a pin that
  // resolved later — the workspace attaching or loading AFTER the providers
  // already have is the ordinary sequence, not an edge case.
  it("an AUTOMATIC previousId loses to a pin that arrives later", () => {
    const candidates = [claude, gateway];
    const result = resolveProviderSelection({
      candidates,
      agent: CLAUDE_CODE,
      agentLabel: "Claude Code",
      previousId: claude.id, // this function's own earlier R1/R2 adoption
      previousName: claude.name,
      agentChanged: false,
      previousExplicit: false,
      pin: gateway.id,
    });
    expect(result).toEqual({ selectedId: gateway.id, changeNote: null });
  });

  it("an EXPLICIT previousId still wins over a pin — a real human choice is never second-guessed", () => {
    const candidates = [claude, gateway];
    const result = resolveProviderSelection({
      candidates,
      agent: CLAUDE_CODE,
      agentLabel: "Claude Code",
      previousId: claude.id, // a real onModelProviderChange call
      previousName: claude.name,
      agentChanged: false,
      previousExplicit: true,
      pin: gateway.id,
    });
    expect(result).toEqual({ selectedId: claude.id, changeNote: null });
  });

  // The same root cause's other half: an automatic pick riding an agent
  // switch into an agent whose own default is disabled must not bypass R5c's
  // "never auto-carried" rule just because it still technically serves.
  it("an agent switch into a default-off agent does not carry the automatic pick", () => {
    const result = resolveProviderSelection({
      candidates: [claude], // claude still serves the new agent
      agent: CLAUDE_CODE,
      agentLabel: "Claude Code",
      previousId: claude.id,
      previousName: claude.name,
      agentChanged: true,
      previousExplicit: false,
      defaultDisabled: true, // the new agent's own named default is off
    });
    expect(result).toEqual({ selectedId: undefined, changeNote: null });
  });

  // #542 rail-gap packet — R5c: a disabled default must never be replaced by
  // whichever OTHER candidate happened to survive the disabled filter.
  it("disabled default is not replaced", () => {
    const result = resolveProviderSelection({
      candidates: [claude], // the sole SURVIVING candidate — NOT the roster default
      agent: CLAUDE_CODE,
      agentLabel: "Claude Code",
      previousId: undefined,
      previousName: undefined,
      agentChanged: false,
      defaultDisabled: true, // gateway, the real default, is off — excluded from `candidates` already
    });
    expect(result).toEqual({ selectedId: undefined, changeNote: null });
  });

  it("with no disabled default, the ordinary sole-survivor rule still applies", () => {
    const result = resolveProviderSelection({
      candidates: [claude],
      agent: CLAUDE_CODE,
      agentLabel: "Claude Code",
      previousId: undefined,
      previousName: undefined,
      agentChanged: false,
      defaultDisabled: false,
    });
    expect(result).toEqual({ selectedId: claude.id, changeNote: null });
  });
});

describe("providerGate — R5b/R5c (#542 rail-gap packet)", () => {
  const CLAUDE_CODE = "claude-code";

  it("is undefined when nothing serves this agent at all (R9)", () => {
    expect(providerGate([], CLAUDE_CODE)).toBeUndefined();
    expect(providerGate([{ ...gateway, harnesses: ["codex-cli"] }], CLAUDE_CODE)).toBeUndefined();
  });

  it("is undefined with an ordinary, non-disabled candidate set", () => {
    expect(providerGate([gateway, claude], CLAUDE_CODE)).toBeUndefined();
  });

  it("names the disabled default (R5c), even with other candidates surviving", () => {
    const disabledDefault = { ...gateway, disabled: true, default_for: [CLAUDE_CODE] };
    expect(providerGate([disabledDefault, claude], CLAUDE_CODE)).toEqual({
      kind: "default_off",
      provider: disabledDefault,
    });
  });

  // Opus review round 2 — R5b is NOT drawn: an all-disabled roster with no
  // NAMED default is R9's shape (undefined), same as the server's own
  // chooseModelProvider (len(serving)==0 launches on the silent advisory,
  // never a refusal) — see providerGate's own doc comment for why the console
  // has no "granted none" signal to read in the first place.
  it("an all-disabled roster with no named default is R9's shape, not a gate", () => {
    expect(providerGate([{ ...gateway, disabled: true }], CLAUDE_CODE)).toBeUndefined();
    expect(providerGate([{ ...gateway, disabled: true }, { ...anthropicKey, disabled: true }], CLAUDE_CODE)).toBeUndefined();
  });
});
