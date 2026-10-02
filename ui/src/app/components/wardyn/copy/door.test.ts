/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { providerStateWord } from "../../screens/new-run/model-provider-lane";
import { BANNER, CONNECTIONS } from "./door";
import { RAIL_PROVIDER } from "./new-run-rail";

// #1489: a model credential that is not there looks the same whether it was
// never added, was deleted when an admin changed the provider's address, or
// could not be read, so every surface says only that none is AVAILABLE. These
// are the approved strings (console-085-packet, Q12), character for character;
// a stored key keeps its word "added".
describe("model connection unavailable — cause-neutral wording", () => {
  it("the account card chip", () => {
    expect(CONNECTIONS.NO_KEY).toBe("No key available");
    expect(CONNECTIONS.NO_TOKEN).toBe("No token available");
  });
  it("the setup strip and Getting started (B4)", () => {
    expect(BANNER.B4("Claude Code", "Anthropic API", false)).toBe("Claude Code runs use Anthropic API, and no key is available.");
    expect(BANNER.B4("Claude Code", "Corp gateway", true)).toBe("Claude Code runs use Corp gateway, and no token is available.");
  });
  it("the launch door's line and picker word", () => {
    expect(RAIL_PROVIDER.NO_KEY("Anthropic API")).toBe("No key is available for Anthropic API.");
    expect(RAIL_PROVIDER.NO_TOKEN("Corp gateway")).toBe("No token is available for Corp gateway.");
    expect(providerStateWord("anthropic_api_key", "not_configured")).toBe("not available");
    expect(providerStateWord("anthropic_api_key", "live")).toBe("added");
  });
});
