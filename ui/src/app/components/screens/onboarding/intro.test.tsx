/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import type { SetupStatus } from "../../../lib/types";
import { hasLlmPath, deriveReadiness, defaultAgentRow, HowItWorksStrip } from "./intro";
import { baseStatus } from "../setup/test-fixtures";

// A minimal-but-valid SetupStatus. The default `make setup` config is a single
// `fake` (deterministic stub) composer backend + no CLI login + no key secret —
// the case that must NOT read as real LLM access anywhere in the funnel. This
// suite's own pins: ready, CC1-only runner, a non-durable-loopback local auth,
// an enabled/`dev` composer, no providers, and a durable secret store.
function status(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return baseStatus({
    ready: true,
    runner: { driver: "docker", confinement_classes: ["CC1"] },
    auth: { mode: "local", local_loopback: false },
    composer: { enabled: true, default: "dev", backends: [] },
    providers: [],
    age_key: { durable: true },
    platform: { os: "linux", wsl: false },
    ...overrides,
  });
}

const fakeBackend = {
  name: "dev",
  provider: "fake",
  model: "demo",
  wire: "fake",
  enabled: true,
  needs_key: false,
  key_resolved: true,
};

// llmReady/llmLabel/composerReady now read the same integration rows
// /integrations itself derives (lib/api/integrations.ts) instead of a bespoke
// heuristic over raw SetupStatus fields — see intro.tsx's own header comment
// for why the fake-backend honesty guard survives this unchanged.
describe("hasLlmPath — honesty guard for the fake composer backend, now via the integrations adapter", () => {
  it("does NOT count a fake-only backend as LLM access (default make setup config)", () => {
    expect(hasLlmPath(status({ composer: { enabled: true, default: "dev", backends: [fakeBackend] } }))).toBe(false);
  });

  it("counts a logged-in CLI (auth_mode: subscription — deriveAiRows' own signal for a resident login)", () => {
    expect(
      hasLlmPath(
        status({
          providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }],
          composer: { enabled: true, default: "dev", backends: [fakeBackend] },
        }),
      ),
    ).toBe(true);
  });

  // UI-LIB-3: auth_mode is only set once Wardyn peeks a real subscription
  // token (setup.go's subOK) — but the server's own llm_provider check calls
  // ANY logged-in CLI real access. This used to read false here while the
  // same payload's llm_provider check read "ok".
  it("counts a resident CLI login even when auth_mode can't confirm a subscription", () => {
    expect(hasLlmPath(status({ providers: [{ tool: "claude", installed: true, logged_in: true }] }))).toBe(true);
    expect(
      hasLlmPath(
        status({ providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "api_key" }] }),
      ),
    ).toBe(true);
  });

  it("counts an anthropic key secret", () => {
    expect(hasLlmPath(status({ secrets: { present: ["anthropic-api-key"], github_app: false } }))).toBe(true);
  });

  it("counts an openai key secret — Codex CLI is an agent tool too", () => {
    expect(hasLlmPath(status({ secrets: { present: ["openai-api-key"], github_app: false } }))).toBe(true);
  });

  it("counts a Wardyn-managed subscription captured via container login", () => {
    expect(hasLlmPath(status({ harness: [{ provider: "anthropic", captured: true }] }))).toBe(true);
  });

  it("counts a fully-resolved Bedrock lane (region + model + a credential source)", () => {
    expect(
      hasLlmPath(status({ bedrock: { region: "us-east-1", model: "anthropic.claude-3", creds_present: true } })),
    ).toBe(true);
  });

  it("does NOT count Bedrock with no region/model set at all", () => {
    expect(hasLlmPath(status({ bedrock: { creds_present: true } }))).toBe(false);
  });

  // Semantic shift from the old five-way heuristic: readiness now agrees with
  // whatever /integrations itself would show for this row (posture reads
  // "Configured" once region+model are both set — deriveAiRows doesn't
  // separately require a credential SOURCE for that specific posture), rather
  // than re-deriving a stricter, independent check. Single source of truth,
  // not a split-brain with the real Integrations page.
  it("counts Bedrock once region+model are set, even with no credential source detected yet", () => {
    expect(
      hasLlmPath(status({ bedrock: { region: "us-east-1", model: "anthropic.claude-3", creds_present: false } })),
    ).toBe(true);
  });

  it("Azure never counts — X_AZURE_HARNESS is a stated fact, not a toggle (composer-only capability)", () => {
    expect(
      hasLlmPath(
        status({
          composer: {
            enabled: true,
            default: "az",
            backends: [
              { name: "az", provider: "azure", model: "gpt-4o", wire: "azure_openai", enabled: true, needs_key: true, key_secret: "azure-openai-key", key_resolved: true },
            ],
          },
        }),
      ),
    ).toBe(false);
  });
});

describe("deriveReadiness — must not overclaim a fake backend as a connected model", () => {
  it("fake-only: llmReady/composerReady false and no label", () => {
    const r = deriveReadiness(status({ composer: { enabled: true, default: "dev", backends: [fakeBackend] } }));
    expect(r.llmReady).toBe(false);
    expect(r.llmLabel).toBe("");
    expect(r.composerReady).toBe(false);
  });

  it("an Anthropic API key is the default agent-tool + Wardyn-features holder — llmLabel names the row", () => {
    const r = deriveReadiness(status({ secrets: { present: ["anthropic-api-key"], github_app: false } }));
    expect(r.llmReady).toBe(true);
    expect(r.llmLabel).toBe("Anthropic (API key)");
    expect(r.composerReady).toBe(true);
  });

  it("a managed (container-login) Claude subscription powers both agent runs and Wardyn features", () => {
    const r = deriveReadiness(status({ harness: [{ provider: "anthropic", captured: true }] }));
    expect(r.llmReady).toBe(true);
    expect(r.llmLabel).toBe("Claude subscription (managed)");
    expect(r.composerReady).toBe(true);
  });

  it("a host-CLI Claude subscription powers agent runs but NOT Wardyn features (opt-in, off by default)", () => {
    const r = deriveReadiness(
      status({ providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }] }),
    );
    expect(r.llmReady).toBe(true);
    expect(r.llmLabel).toBe("Claude subscription (host CLI)");
    expect(r.composerReady).toBe(false);
  });

  it("a resident CLI login with an unconfirmed auth_mode still powers agent runs, honestly labeled apart from a confirmed subscription", () => {
    const r = deriveReadiness(status({ providers: [{ tool: "claude", installed: true, logged_in: true }] }));
    expect(r.llmReady).toBe(true);
    expect(r.llmLabel).toBe("Claude Code CLI (resident login)");
    expect(r.composerReady).toBe(false);
  });

  it("Azure powers Wardyn features (composerReady) but never counts as an agent-tool path (llmReady)", () => {
    const r = deriveReadiness(
      status({
        composer: {
          enabled: true,
          default: "az",
          backends: [
            { name: "az", provider: "azure", model: "gpt-4o", wire: "azure_openai", enabled: true, needs_key: true, key_secret: "azure-openai-key", key_resolved: true },
          ],
        },
      }),
    );
    expect(r.llmReady).toBe(false);
    expect(r.composerReady).toBe(true);
  });
});

// HIGH-4 review fix: a member's redacted SetupStatus (Go redactSetupStatusForMember)
// zeroes runner.confinement_classes and drops the providers/secrets detail
// deriveAiRows reads — without these two fallbacks, deriveReadiness would read a
// member as permanently un-ready no matter the server's real state.
describe("deriveReadiness — member-redaction survivors (HIGH-4)", () => {
  it("prefers status.llm_ready over the locally-derived agent-row check when the server sent one", () => {
    // No secrets/providers/harness at all (a redacted member response) would
    // locally derive llmReady=false — but the server's own pre-redaction
    // verdict says otherwise, and it must win.
    const r = deriveReadiness(status({ llm_ready: true }));
    expect(r.llmReady).toBe(true);
  });

  it("still honors a locally-derived false when the server explicitly says llm_ready: false", () => {
    const r = deriveReadiness(status({ secrets: { present: ["anthropic-api-key"], github_app: false }, llm_ready: false }));
    expect(r.llmReady).toBe(false);
  });

  it("falls back to the locally-derived value when llm_ready is absent (older daemon / READY_FALLBACK)", () => {
    const r = deriveReadiness(status({ secrets: { present: ["anthropic-api-key"], github_app: false } }));
    expect(r.llmReady).toBe(true);
  });

  it("falls back barrierReady to status.ready when confinement_classes is empty (redacted member, real backend)", () => {
    const r = deriveReadiness(status({ ready: true, runner: { driver: "docker", confinement_classes: [] } }));
    expect(r.barrierReady).toBe(true);
    expect(r.barrierCount).toBe(0);
  });

  it("an empty confinement_classes with ready:false stays not-ready (genuinely unconfigured, not just redacted)", () => {
    const r = deriveReadiness(status({ ready: false, runner: { driver: "docker", confinement_classes: [] } }));
    expect(r.barrierReady).toBe(false);
  });
});

// defaultAgentRow — the ROW deriveReadiness's llmLabel is built from, exported
// so a caller that needs the row itself (not just its name) can reuse the same
// pick instead of re-deriving it (demos/harness-demo.ts's binding picker).
describe("defaultAgentRow — the same pick llmLabel is built from", () => {
  it("is undefined with nothing agent-capable connected", () => {
    expect(defaultAgentRow(status({ composer: { enabled: true, default: "dev", backends: [fakeBackend] } }))).toBeUndefined();
  });

  it("names the row whose .name matches llmLabel for a connected Anthropic key", () => {
    const s = status({ secrets: { present: ["anthropic-api-key"], github_app: false } });
    const row = defaultAgentRow(s);
    expect(row?.id).toBe("ai:anthropic_api_key");
    expect(row?.name).toBe(deriveReadiness(s).llmLabel);
  });
});

describe("HowItWorksStrip — node 5 qualifier is design-law verbatim", () => {
  it("renders the exact append-only audit qualifier string", () => {
    render(<HowItWorksStrip />);
    expect(
      screen.getByText("Append-only audit; session replay where the runner supports it"),
    ).toBeInTheDocument();
  });
});
