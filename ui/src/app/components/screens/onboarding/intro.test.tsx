/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import type { SetupStatus } from "../../../lib/types";
import { hasLlmPath, deriveReadiness, HowItWorksStrip } from "./intro";

// A minimal-but-valid SetupStatus. The default `make setup` config is a single
// `fake` (deterministic stub) composer backend + no CLI login + no key secret —
// the case that must NOT read as real LLM access anywhere in the funnel.
function status(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return {
    ready: true,
    checks: [],
    auth: { mode: "local", local_loopback: false },
    runner: { driver: "docker", confinement_classes: ["CC1"] },
    composer: { enabled: true, default: "dev", backends: [] },
    providers: [],
    secrets: { present: [], github_app: false },
    age_key: { durable: true },
    has_runs: false,
    platform: { os: "linux", wsl: false },
    ...overrides,
  };
}

const fakeBackend = {
  name: "dev", provider: "fake", model: "demo", wire: "fake",
  enabled: true, needs_key: false, key_resolved: true,
};
const realBackend = {
  name: "primary", provider: "anthropic", model: "m", wire: "anthropic",
  enabled: true, needs_key: true, key_secret: "anthropic-api-key", key_resolved: true,
};

describe("hasLlmPath — honesty guard for the fake composer backend", () => {
  it("does NOT count a fake-only backend as LLM access (default make setup config)", () => {
    expect(hasLlmPath(status({ composer: { enabled: true, default: "dev", backends: [fakeBackend] } }))).toBe(false);
  });
  it("counts a real resolved backend", () => {
    expect(hasLlmPath(status({ composer: { enabled: true, default: "primary", backends: [realBackend] } }))).toBe(true);
  });
  it("counts a logged-in CLI even alongside a fake backend", () => {
    expect(
      hasLlmPath(
        status({
          providers: [{ tool: "claude", installed: true, logged_in: true }],
          composer: { enabled: true, default: "dev", backends: [fakeBackend] },
        }),
      ),
    ).toBe(true);
  });
  it("counts an anthropic key secret", () => {
    expect(hasLlmPath(status({ secrets: { present: ["anthropic-api-key"], github_app: false } }))).toBe(true);
  });
});

describe("deriveReadiness — must not overclaim a fake backend as a connected model", () => {
  it("fake-only: llmReady false and NO 'Composer backend ready' label", () => {
    const r = deriveReadiness(status({ composer: { enabled: true, default: "dev", backends: [fakeBackend] } }));
    expect(r.llmReady).toBe(false);
    expect(r.llmLabel).toBe("");
  });
  it("real backend: llmReady true with the composer label", () => {
    const r = deriveReadiness(status({ composer: { enabled: true, default: "primary", backends: [realBackend] } }));
    expect(r.llmReady).toBe(true);
    expect(r.llmLabel).toBe("Composer backend ready");
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
