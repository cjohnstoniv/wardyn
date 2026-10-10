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
import { BANNER, CONNECTIONS } from "./copy/door";
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

  // #993: the server grades a session for another account/role than the
  // provider pins expired_signin with an action naming both pairs; the strip
  // said only "no longer works" and dropped it.
  it("shows the server action for a pin-contradicted AWS session", () => {
    const action =
      "Your stored AWS session is for account 999999999999 / role Wrong; this row now allows 123456789012 / BedrockUser — sign in again.";
    const { unmount } = strip(providerStatus([{ provider: bedrock, state: "expired_signin", action }]));
    expect(screen.getByText("Your AWS sign-in for Bedrock (prod) no longer works.")).toBeInTheDocument();
    expect(screen.getByText(action)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeInTheDocument();
    unmount();
    // A plain dead session's action is the button's own label: never printed as prose.
    strip(providerStatus([{ provider: bedrock, state: "expired_signin", action: AGENTS.SIGN_IN_AWS }]));
    expect(screen.getAllByText(AGENTS.SIGN_IN_AWS)).toHaveLength(1);
  });

  it("B4: a default token not available, for both agents it serves; a key reads 'key'", async () => {
    const { unmount } = strip(providerStatus([{ provider: gateway, defaultFor: ["claude-code", "codex-cli"] }]));
    expect(screen.getByText(BANNER.B4("Claude Code and Codex CLI", "Corp gateway", true))).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Add your token" }));
    expect(await screen.findByRole("dialog", { name: "Add your token for Corp gateway" })).toBeInTheDocument();
    unmount();
    strip(providerStatus([{ provider: anthropicKey, defaultFor: ["claude-code"] }]));
    expect(screen.getByText("Claude Code runs use Anthropic API key, and no key is available.")).toBeInTheDocument();
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

  it("with no provider block, providerAttention is empty", () => {
    expect(providerAttention(baseStatus())).toEqual([]);
  });
});

describe("providerStripLine — the kind/state table", () => {
  it("draws nothing for an expiring sign-in with no deadline to name", () => {
    expect(
      providerStripLine({ provider: bedrock, state: "expiring", deadline: "", action: "", defaultFor: [] }, ""),
    ).toBeNull();
  });
});


describe("provider strip connection causes", () => {
  it.each([bedrock, claude, gateway, anthropicKey])("store read failure for $kind offers only re-check", async (provider) => {
    const status = providerStatus([{ provider, defaultFor: ["claude-code"] }]);
    status.provider_access![0].cause = "store_unreadable";
    const refresh = vi.fn();
    render(<WithDoor status={status} path="/runs" operator={false} onRefresh={refresh} />);
    expect(screen.getByText(CONNECTIONS.STORE_UNREADABLE)).toBeInTheDocument();
    expect(screen.queryByText(/not signed in|no key is available/i)).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: CONNECTIONS.RECHECK }));
    expect(refresh).toHaveBeenCalledOnce();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it.each(["never_connected", "destination_changed", "kind_changed"])("%s carries the cause through the attention row", (cause) => {
    const status = providerStatus([{ provider: gateway, defaultFor: ["claude-code"] }]);
    Object.assign(status.provider_access![0], { cause, new_destination: "new.example" });
    strip(status);
    const line = cause === "never_connected" ? CONNECTIONS.NEVER_CONNECTED("new.example", gateway.name || gateway.id)
      : cause === "destination_changed" ? CONNECTIONS.DESTINATION_CHANGED("new.example") : CONNECTIONS.KIND_CHANGED("new.example");
    expect(screen.getByText(line)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: cause === "never_connected" ? CONNECTIONS.ADD_TOKEN : CONNECTIONS.REVIEW_RECONNECT })).toBeInTheDocument();
  });
});
