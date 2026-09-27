/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The shell strip under a model-provider block (#540, packet MP-D §5.5): one
// line per provider that needs the person, B8 from two, the door each line
// opens keyed by its provider, and the User view only.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("../screens/settings/harness-login-pane", () => ({
  HarnessLoginPane: (p: { modelProvider?: string }) => (
    <div data-testid="fake-pane" data-model-provider={p.modelProvider ?? ""} />
  ),
}));

import { providerStripLine } from "./model-access-banner";
import { MODEL_ACCESS_BANNER } from "./model-access-copy";
import { BANNER } from "./copy/door";
import { AGENTS } from "../../lib/workspace-providers-copy";
import { providerAttention } from "../../lib/model-access";
import { aheadByHours } from "../../lib/test-clock";
import { MODEL_PROVIDERS, baseStatus, providerStatus } from "../../lib/test-fixtures";
import type { SetupHarnessTool, SetupStatus } from "../../lib/types";
import { WithDoor } from "../../../test/door-harness";

const { bedrock, claude, gateway, anthropicKey } = MODEL_PROVIDERS;
const HARNESSES: SetupHarnessTool[] = [
  { id: "claude-code", display: "Claude Code", has_gateway: false, has_login: true },
  { id: "codex-cli", display: "Codex CLI", has_gateway: false, has_login: true },
];

function strip(status: SetupStatus, path = "/runs") {
  return render(<WithDoor status={{ ...status, harnesses: HARNESSES }} path={path} operator={false} />);
}

beforeEach(() => {
  try {
    window.sessionStorage.clear();
  } catch {
    /* jsdom always has it */
  }
});
afterEach(() => vi.restoreAllMocks());

describe("B1–B5: one provider needs the person", () => {
  it("B1: a default AWS sign-in not signed in — the door for THAT provider, and Not now", async () => {
    strip(providerStatus([{ provider: bedrock, defaultFor: ["claude-code"] }]));
    expect(screen.getByText(BANNER.B1("Claude Code", "Bedrock (prod)"))).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
    expect(await screen.findByTestId("fake-pane")).toHaveAttribute("data-model-provider", "bedrock-prod");
  });

  it("B1's Not now is per provider and per viewer", async () => {
    strip(providerStatus([{ provider: bedrock, defaultFor: ["claude-code"] }]));
    await userEvent.click(screen.getByRole("button", { name: MODEL_ACCESS_BANNER.NOT_NOW }));
    expect(screen.queryByText(BANNER.B1("Claude Code", "Bedrock (prod)"))).toBeNull();
    expect(window.sessionStorage.getItem("wardyn.modelAccessDismissed.someone@corp.example.bedrock-prod")).toBe("1");
  });

  it("B2 / B3: a held AWS sign-in dead or lapsing, no Not now", () => {
    const { unmount } = strip(providerStatus([{ provider: bedrock, state: "expired_signin" }]));
    expect(screen.getByText("Your AWS sign-in for Bedrock (prod) no longer works.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: MODEL_ACCESS_BANNER.NOT_NOW })).toBeNull();
    unmount();
    strip(providerStatus([{ provider: bedrock, state: "expiring", deadline: aheadByHours(3) }]));
    expect(screen.getByText(/^Your AWS sign-in for Bedrock \(prod\) lapses in /)).toBeInTheDocument();
  });

  it("B4: a default token not added, for both agents it serves; a key reads 'key'", async () => {
    const { unmount } = strip(providerStatus([{ provider: gateway, defaultFor: ["claude-code", "codex-cli"] }]));
    expect(screen.getByText(BANNER.B4("Claude Code and Codex CLI", "Corp gateway", true))).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Add your token" }));
    expect(await screen.findByRole("dialog", { name: "Add your token for Corp gateway" })).toBeInTheDocument();
    unmount();
    strip(providerStatus([{ provider: anthropicKey, defaultFor: ["claude-code"] }]));
    expect(screen.getByText("Claude Code runs use Anthropic API key, and you haven't added your key.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add your key" })).toBeInTheDocument();
  });

  it("B5: a default Claude subscription not signed in", () => {
    strip(providerStatus([{ provider: claude, defaultFor: ["claude-code"] }]));
    expect(screen.getByText(BANNER.B5("Claude Code"))).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sign in to Claude" })).toBeInTheDocument();
  });
});

describe("B8, and where the strip is silent", () => {
  it("two or more collapse to a count with Review, to /account", () => {
    strip(
      providerStatus([
        { provider: bedrock, defaultFor: ["claude-code"] },
        { provider: gateway, defaultFor: ["codex-cli"] },
      ]),
    );
    expect(screen.getByText(BANNER.B8(2))).toBeInTheDocument();
    expect(screen.getByRole("link", { name: BANNER.REVIEW })).toHaveAttribute("href", "/account");
  });

  it("case (d): a connected default and an unused key raise nothing", () => {
    strip(
      providerStatus([
        { provider: bedrock, defaultFor: ["claude-code"], state: "live" },
        { provider: anthropicKey },
      ]),
    );
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("the Admin view shows no provider strip (packet MP-D: Member view only)", () => {
    strip(providerStatus([{ provider: bedrock, defaultFor: ["claude-code"] }]), "/admin/runs");
    expect(screen.queryByText(BANNER.B1("Claude Code", "Bedrock (prod)"))).toBeNull();
  });

  it("with a provider block the legacy AWS sentence never renders beside it", () => {
    strip(
      providerStatus([{ provider: bedrock, defaultFor: ["claude-code"], state: "live" }], {
        model_access: { state: "not_configured", action: AGENTS.SIGN_IN_AWS },
      }),
    );
    expect(screen.queryByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeNull();
  });

  it("with no provider block, providerAttention is empty and today's strip speaks", () => {
    expect(providerAttention(baseStatus())).toEqual([]);
  });
});

describe("providerStripLine — the kind/state table", () => {
  it("draws nothing for an expiring sign-in with no deadline to name", () => {
    expect(
      providerStripLine({ provider: bedrock, state: "expiring", deadline: "", defaultFor: [] }, ""),
    ).toBeNull();
  });
});
