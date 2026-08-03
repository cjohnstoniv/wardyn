/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import {
  T,
  CAPS,
  CATEGORY_META,
  AI_TYPES,
  RESIDENCY_META,
  SUBSCRIPTION_LANE_META,
  BEDROCK_LANE_META,
  IMPOSSIBLE,
  TOOLS,
} from "./integrations";

// Sentinel byte-exact pins against the approved mock export
// (mockup/wardyn-integrations.js `T`/`CAPS`) — em-dashes, the curly single
// quotes in BLAST[1], and the middle dot in CAPS.azure()'s label are all
// significant and easy to flatten by hand-retyping.
describe("integrations — T canon sentinel pins", () => {
  it("pins plain entries verbatim, em-dashes included", () => {
    expect(T.LEDE).toBe(
      "Named connections to systems outside Wardyn — model providers, git hosts, artifact mirrors, your corporate proxy. Wardyn runs without any of them.",
    );
    expect(T.LAW).toBe(
      "A tool is what the image carries. An integration is what it connects through. Wardyn never installs tools into your image — it only wires them at run time.",
    );
    expect(T.X_SUB_DIRECT).toBe(
      "A subscription token is accepted only for Claude-Code-shaped requests; anything else comes back 429. That's Anthropic's gate, not a Wardyn setting.",
    );
  });

  it("pins BLAST verbatim, including the curly quotes around 'Describe your task'", () => {
    expect(T.BLAST).toHaveLength(4);
    expect(T.BLAST[1]).toBe("Wardyn's Composer loses its backend — ‘Describe your task’ disappears from New Run.");
    expect(T.BLAST[3]).toBe("The stored secret anthropic-api-key is not deleted — remove it under Secrets.");
  });
});

describe("integrations — CAPS capability-line notes", () => {
  it("pins a fact line (impossible-as-fact, never a toggle) verbatim", () => {
    expect(CAPS.key()[1]).toEqual({ label: "Codex CLI", fact: T.X_KEY_CODEX });
  });

  it("pins the middle dot in azure()'s combined label", () => {
    expect(CAPS.azure()[0].label).toBe("Claude Code · Codex CLI");
  });

  it("sub(hostCli) is the only row set that varies by argument (Wardyn features)", () => {
    const off = CAPS.sub(false).find((r) => r.label === "Wardyn features");
    const on = CAPS.sub(true).find((r) => r.label === "Wardyn features");
    expect(off?.on).toBe(true);
    expect(on?.on).toBe(false);
  });
});

describe("integrations — structured metadata is grounded in the T/CAPS canon above", () => {
  it("CATEGORY_META's skip-if line IS T.CAT_* (same string, not a re-typed copy)", () => {
    expect(CATEGORY_META.ai_provider.skipIfLine).toBe(T.CAT_AI);
    expect(CATEGORY_META.scm_host.skipIfLine).toBe(T.CAT_SCM);
    expect(CATEGORY_META.artifact_mirror.skipIfLine).toBe(T.CAT_MIRROR);
    expect(CATEGORY_META.host_proxy.skipIfLine).toBe(T.CAT_PROXY);
  });

  it("AI_TYPES.desc IS the matching T.TY_* for the three single-lane key types", () => {
    expect(AI_TYPES.anthropic_api_key.desc).toBe(T.TY_KEY);
    expect(AI_TYPES.openai_api_key.desc).toBe(T.TY_OPENAI);
    expect(AI_TYPES.azure_openai.desc).toBe(`${T.TY_AZURE} Key or Entra.`);
  });

  it("AI_TYPES.capabilityPreview wires straight to CAPS (no duplicated tables)", () => {
    expect(AI_TYPES.anthropic_api_key.capabilityPreview()).toEqual(CAPS.key());
    expect(AI_TYPES.bedrock.capabilityPreview()).toEqual(CAPS.bedrock());
    expect(AI_TYPES.anthropic_subscription.capabilityPreview(true)).toEqual(CAPS.sub(true));
  });

  it("IMPOSSIBLE reasons are the exact same T.X_* strings CAPS' fact rows use", () => {
    expect(IMPOSSIBLE.anthropic_api_key?.codex_cli).toBe(T.X_KEY_CODEX);
    expect(IMPOSSIBLE.anthropic_subscription?.direct_api).toBe(T.X_SUB_DIRECT);
    expect(IMPOSSIBLE.azure_openai?.claude_code).toBe(T.X_AZURE_HARNESS);
    expect(IMPOSSIBLE.azure_openai?.codex_cli).toBe(T.X_AZURE_HARNESS);
  });

  it("SUBSCRIPTION_LANE_META tooltips ARE T.MANAGED_LINE / T.HOSTCLI_LINE verbatim", () => {
    expect(SUBSCRIPTION_LANE_META.managed.tooltip).toBe(T.MANAGED_LINE);
    expect(SUBSCRIPTION_LANE_META.resident_host.tooltip).toBe(T.HOSTCLI_LINE);
  });

  it("RESIDENCY_META covers all six kinds with a label + tone + tooltip", () => {
    for (const kind of [
      "proxy_injected",
      "brokered_mint",
      "resident_mount",
      "resident_env",
      "control_plane",
      "varies",
    ] as const) {
      expect(RESIDENCY_META[kind].label).toBeTruthy();
      expect(RESIDENCY_META[kind].tooltip).toBeTruthy();
      expect(["success", "warning", "neutral"]).toContain(RESIDENCY_META[kind].tone);
    }
  });

  // control_plane is distinct from `varies`: Azure has exactly ONE lane (the
  // mock's "cp" resChip kind), never "more than one lane" — the two kinds must
  // not collapse to the same label/tooltip.
  it("control_plane reads distinctly from varies (Azure has one lane, not several)", () => {
    expect(RESIDENCY_META.control_plane.label).toBe("control-plane side");
    expect(RESIDENCY_META.control_plane.label).not.toBe(RESIDENCY_META.varies.label);
  });

  it("TOOLS covers all six Tools-tab rows, verbatim against the mock's toolRow calls", () => {
    expect(TOOLS.git.wire).toBe(
      "Clone and push rerouted through the broker (App), a credential helper (PAT), or a key file (SSH) — set up at run start.",
    );
    expect(TOOLS.git.extra).toBe("In every Wardyn image.");
    expect(TOOLS.package_managers.wire).toBe(
      "Per-tool config files generated at run start (.npmrc, pip.conf, …); the mirror token is injected proxy-side.",
    );
    expect(TOOLS.gh_cli.wire).toBe(
      "Recognized on the host, never wired. Its token is broad; Wardyn never imports it — use an SCM host integration instead.",
    );
    for (const id of ["git", "package_managers", "claude_code", "codex_cli", "gh_cli", "own_tools"] as const) {
      expect(TOOLS[id].wire).toBeTruthy();
    }
  });

  it("BEDROCK_LANE_META covers all four lanes and points each at a residency kind", () => {
    for (const lane of ["bearer", "sso", "aws_dir", "static"] as const) {
      expect(RESIDENCY_META[BEDROCK_LANE_META[lane].residency]).toBeDefined();
    }
    // Raw access keys are env vars, not a mounted file — the one lane that
    // differs from its "resident" siblings.
    expect(BEDROCK_LANE_META.static.residency).toBe("resident_env");
    expect(BEDROCK_LANE_META.aws_dir.residency).toBe("resident_mount");
  });
});
