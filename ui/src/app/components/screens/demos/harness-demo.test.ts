/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { harnessAvailability } from "./harness-demo";
import { baseStatus } from "../setup/test-fixtures";

describe("harnessAvailability", () => {
  it("is 'none' with nothing agent-capable connected (locked state)", () => {
    expect(harnessAvailability(baseStatus())).toBe("none");
  });

  it("is 'claude_ready' for an Anthropic API key", () => {
    expect(
      harnessAvailability(baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } })),
    ).toBe("claude_ready");
  });

  it("is 'openai_only' when the only agent-capable row is an OpenAI key — Codex CLI isn't supported here", () => {
    expect(
      harnessAvailability(baseStatus({ secrets: { present: ["openai-api-key"], github_app: false } })),
    ).toBe("openai_only");
  });

  // Push order in deriveAiRows always lists a Claude-capable type before
  // openai_api_key, so the pick prefers it whenever both are connected.
  it("prefers a co-connected Claude-capable row over an OpenAI one", () => {
    expect(
      harnessAvailability(
        baseStatus({ secrets: { present: ["anthropic-api-key", "openai-api-key"], github_app: false } }),
      ),
    ).toBe("claude_ready");
  });

  it("is 'claude_ready' for a Wardyn-managed subscription (no secret, a captured harness login)", () => {
    expect(harnessAvailability(baseStatus({ harness: [{ provider: "anthropic", captured: true }] }))).toBe(
      "claude_ready",
    );
  });

  it("is 'claude_ready' for a resolved Bedrock lane", () => {
    expect(
      harnessAvailability(
        baseStatus({ bedrock: { region: "us-east-1", model: "anthropic.claude-3", creds_present: true } }),
      ),
    ).toBe("claude_ready");
  });
});
