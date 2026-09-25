/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Pins lib/model-providers-copy.ts to canon.html Table 1 (owner-approved
// 2026-09-25), byte for byte: each literal below is the canon row, with the
// row's own fixture filled into its {placeholder}.
import { describe, it, expect } from "vitest";
import { T } from "./integrations";
import { MODEL_PROVIDERS, PROVIDER_EDITOR, PROVIDERS } from "./model-providers-copy";

describe("model-providers canon, Table 1 (#537)", () => {
  it("every string is its canon row", () => {
    expect(MODEL_PROVIDERS.ADD_CTA).toBe("Add model provider");
    expect(MODEL_PROVIDERS.KIND.anthropic_api_key).toBe("Anthropic API key");
    expect(MODEL_PROVIDERS.KIND.openai_api_key).toBe("OpenAI API key");
    expect(MODEL_PROVIDERS.KIND.custom_endpoint).toBe("Your own endpoint");
    expect(MODEL_PROVIDERS.KIND.anthropic_subscription).toBe("Claude subscription");
    expect(MODEL_PROVIDERS.KIND.bedrock_sso).toBe("Amazon Bedrock");

    const E = PROVIDER_EDITOR;
    expect(E.KIND_TITLE).toBe("What kind of model provider?");
    expect(E.PROVIDES_KEY).toBe("Each person adds their own key.");
    expect(E.PROVIDES_TOKEN).toBe("Each person adds their own token. You set how it is sent.");
    expect(E.NAME).toBe("Name");
    expect(E.NAME_HINT).toBe("What people see when they choose it.");
    expect(E.ROUTE_THROUGH).toBe("Route through a gateway (optional)");
    expect(E.ROUTE_THROUGH_HINT("api.anthropic.com")).toBe(
      "Send requests to your gateway instead of api.anthropic.com. Each person's key goes with them, and they are told where it goes.",
    );
    expect(E.BASE_URL).toBe("Base URL");
    expect(E.BASE_URL_HINT).toBe("https only. Wardyn sends this provider's requests here.");
    expect(E.AUTH_HEADER).toBe("Auth header");
    expect(E.VALUE_FORMAT).toBe("Value format");
    expect(E.USE_WITH).toBe("Use with");
    expect(E.MODEL).toBe("Model");
    expect(E.MODEL_HINT).toBe("Leave empty for the agent's own default.");
    expect(E.PATH).toBe("Path");
    expect(E.PATH_HINT_CLAUDE).toBe(
      "Where your endpoint serves the Anthropic Messages API for Claude Code, e.g. /anthropic.",
    );
    expect(E.PATH_HINT_CODEX).toBe("Where your endpoint serves the OpenAI Responses API for Codex CLI, e.g. /v1.");
    expect(E.CANCEL).toBe("Cancel");
    expect(E.SAVE).toBe("Save");
    expect(E.REMOVE).toBe("Remove");
    expect(E.SAVED_TOAST).toBe("Provider saved.");
    expect(PROVIDERS.SAVE_REFUSED_TITLE_ONE).toBe("This provider can't be saved as written");
    expect(E.DELETE_TITLE("Corp gateway")).toBe("Remove Corp gateway?");
    expect(E.DELETE_BODY).toBe(
      "Runs that chose it are refused until they choose another. Everyone's tokens for it are deleted.",
    );
    expect(E.DELETE_BODY_KEY).toBe(
      "Runs that chose it are refused until they choose another. Everyone's keys for it are deleted.",
    );
    expect(E.DELETE_BLOCKED("Corp gateway", "Claude Code")).toBe(
      "Corp gateway is the default for Claude Code — choose another default first.",
    );
    expect(E.ADDRESS_TITLE("Corp gateway")).toBe("Change where Corp gateway sends requests?");
    expect(E.ADDRESS_BODY(12)).toBe(
      "The tokens 12 people added were given for the old address, so they are deleted. Everyone who connected adds theirs again.",
    );
    expect(E.ADDRESS_BODY_ONE).toBe(
      "The token 1 person added was given for the old address, so it is deleted. They add it again.",
    );
    expect(E.ADDRESS_BODY_KEY(12)).toBe(
      "The keys 12 people added were given for the old address, so they are deleted. Everyone who connected adds theirs again.",
    );
    expect(E.ADDRESS_BODY_KEY_ONE).toBe(
      "The key 1 person added was given for the old address, so it is deleted. They add it again.",
    );
  });

  it("the two reused catalog reasons are still integrations.ts's rows", () => {
    expect(T.X_KEY_CODEX).toBe("Codex CLI speaks the OpenAI API only — an Anthropic key can't drive it. Not a setting.");
    expect(T.X_OPENAI_CLAUDE).toBe(
      "Claude Code speaks the Anthropic API only — an OpenAI key can't drive it. Not a setting.",
    );
  });
});
