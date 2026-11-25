/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Azure Foundry on a person's side (011-foundry-m7 F10, F11, F12): one row, and one door, per provider row, each
// state's label, and the strip's two sentences.
import { describe, expect, it } from "vitest";
import { providerAttention } from "./model-access";
import { connectionRowCopy, connectionRows, connectionsSummary } from "./model-connections";
import { providerStripLine } from "../components/wardyn/model-access-banner";
import { BANNER, CONNECTIONS } from "../components/wardyn/copy/door";
import { providerStatus } from "./test-fixtures";
import type { SetupModelProvider } from "./types";

const RESPONSES: SetupModelProvider = {
  id: "azure-responses",
  uid: "u-az-1",
  name: "Azure (responses)",
  kind: "azure_foundry",
  harnesses: ["codex-cli"],
  host: "my-resource.openai.azure.com",
};
// A second row on the same endpoint: a second door.
const MESSAGES: SetupModelProvider = { ...RESPONSES, id: "azure-messages", uid: "u-az-2", name: "Azure (messages)", harnesses: ["claude-code"] };

describe("Azure Foundry connection rows", () => {
  it("two rows on one endpoint are two rows, each with its own Sign in to Azure", () => {
    const status = providerStatus([
      { provider: RESPONSES, defaultFor: ["codex-cli"] },
      { provider: MESSAGES, defaultFor: ["claude-code"] },
    ]);
    const rows = connectionRows(status);
    expect(rows.map((r) => r.provider.uid)).toEqual(["u-az-1", "u-az-2"]);
    for (const r of rows) {
      expect(connectionRowCopy(status, r)).toMatchObject({ chip: { label: CONNECTIONS.NOT_SIGNED_IN, tone: "neutral" }, button: CONNECTIONS.SIGN_IN_AZURE, line: "" });
    }
    expect(connectionsSummary(rows).label).toBe(CONNECTIONS.SUMMARY_NEEDS_YOU);
  });

  it("each state reads as its label", () => {
    const copy = (state: string) => {
      const status = providerStatus([{ provider: RESPONSES, state }]);
      return connectionRowCopy(status, connectionRows(status)[0]);
    };
    expect(copy("not_configured")).toMatchObject({ chip: { label: CONNECTIONS.NOT_SIGNED_IN }, button: CONNECTIONS.SIGN_IN_AZURE });
    expect(copy("live")).toMatchObject({ chip: { label: CONNECTIONS.SIGNED_IN, tone: "success" }, button: CONNECTIONS.SIGN_IN_AGAIN, line: "" });
    expect(copy("expired_signin")).toMatchObject({
      chip: { label: CONNECTIONS.SIGNED_OUT, tone: "danger" },
      line: CONNECTIONS.C6_LINE_AZURE("Azure (responses)"),
      button: CONNECTIONS.SIGN_IN_AZURE,
    });
  });
});

describe("the strip for an Azure Foundry default", () => {
  it("says B6 for a default that is not signed in, and the C6 line once the sign-in ended", () => {
    const none = providerStatus([{ provider: RESPONSES, defaultFor: ["codex-cli"] }]);
    expect(providerStripLine(providerAttention(none)[0], "Codex CLI")).toMatchObject({
      sentence: BANNER.B6("Codex CLI", "Azure (responses)"),
      button: CONNECTIONS.SIGN_IN_AZURE,
      dismissible: false,
    });
    const ended = providerStatus([{ provider: RESPONSES, defaultFor: ["codex-cli"], state: "expired_signin" }]);
    expect(providerStripLine(providerAttention(ended)[0], "Codex CLI")).toMatchObject({
      sentence: CONNECTIONS.C6_LINE_AZURE("Azure (responses)"),
      button: CONNECTIONS.SIGN_IN_AZURE,
    });
  });

  it("stays quiet for a signed-in default", () => {
    expect(providerAttention(providerStatus([{ provider: RESPONSES, defaultFor: ["codex-cli"], state: "live" }]))).toEqual([]);
  });
});
