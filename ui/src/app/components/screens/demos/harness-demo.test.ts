/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { pickHarnessBinding } from "./harness-demo";
import { baseStatus } from "../setup/test-fixtures";

describe("pickHarnessBinding", () => {
  it("returns null with nothing agent-capable connected (locked state)", () => {
    expect(pickHarnessBinding(baseStatus())).toBeNull();
  });

  it("binds to the default-holder row and claude-code for an Anthropic key", () => {
    const binding = pickHarnessBinding(
      baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }),
    );
    expect(binding).not.toBeNull();
    expect(binding!.row.id).toBe("ai:anthropic_api_key");
    expect(binding!.agent).toBe("claude-code");
    expect(binding!.integrationId).toBe(binding!.row.id);
  });

  // No client-authored api_key grant: the overlay is exactly {agent, integrationId} —
  // the server authors the grant from the integration id alone.
  it("switches to codex-cli for an OpenAI key, and still names the real integration id", () => {
    const binding = pickHarnessBinding(
      baseStatus({ secrets: { present: ["openai-api-key"], github_app: false } }),
    );
    expect(binding).not.toBeNull();
    expect(binding!.row.id).toBe("ai:openai_api_key");
    expect(binding!.agent).toBe("codex-cli");
    expect(binding!.integrationId).toBe("ai:openai_api_key");
  });

  // Anthropic holds the agent-tool "default" chip when both are connected
  // (lib/integrations.ts's CAPS.key def:true vs CAPS.openai's undefined def) —
  // pickHarnessBinding must prefer it, same as Readiness.llmLabel does.
  it("prefers the default-holder (Anthropic) over a co-connected OpenAI key", () => {
    const binding = pickHarnessBinding(
      baseStatus({ secrets: { present: ["anthropic-api-key", "openai-api-key"], github_app: false } }),
    );
    expect(binding!.row.id).toBe("ai:anthropic_api_key");
    expect(binding!.agent).toBe("claude-code");
  });

  it("binds to a Wardyn-managed subscription (no secret, a captured harness login)", () => {
    const binding = pickHarnessBinding(baseStatus({ harness: [{ provider: "anthropic", captured: true }] }));
    expect(binding).not.toBeNull();
    expect(binding!.row.id).toBe("ai:anthropic_subscription:managed");
    expect(binding!.agent).toBe("claude-code");
  });
});
