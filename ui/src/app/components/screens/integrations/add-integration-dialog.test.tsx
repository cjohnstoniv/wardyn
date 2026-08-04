/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { baseStatus } from "../setup/test-fixtures";
import { T } from "../../../lib/integrations";
import type { IntegrationRow } from "../../../lib/api/integrations";
import { AddIntegrationDialog, type AddIntegrationTarget } from "./add-integration-dialog";

function renderDialog(existingAiRows: IntegrationRow[] = [], target?: AddIntegrationTarget) {
  const reload = vi.fn();
  const onOpenChange = vi.fn();
  render(
    <AddIntegrationDialog
      open
      onOpenChange={onOpenChange}
      status={baseStatus()}
      siteConfig={{}}
      existingAiRows={existingAiRows}
      reload={reload}
      target={target}
    />,
  );
  return { reload, onOpenChange };
}

// The search-first Add flow has already asked what you are connecting. Opening
// on the coarse "AI provider or SCM host?" card grid after that is a step
// BACKWARDS, and reads as a second, different dialog.
describe("AddIntegrationDialog — opening on a handed-off target", () => {
  it("opens straight on the picked provider, skipping the category question", () => {
    renderDialog([], { s: "ai_connect", type: "anthropic_api_key" });
    expect(screen.queryByText("SCM host")).not.toBeInTheDocument();
    expect(screen.queryByText(T.CAT_AI)).not.toBeInTheDocument();
    // Back still reaches the sibling AI types, so nothing is unreachable.
    expect(screen.getByRole("button", { name: /back/i })).toBeInTheDocument();
  });

  it("opens the SCM ladder directly for a git host", () => {
    renderDialog([], { s: "scm" });
    expect(screen.queryByText("AI provider")).not.toBeInTheDocument();
  });

  it("still opens on the category picker when nothing was picked first", () => {
    renderDialog();
    expect(screen.getByText("AI provider")).toBeInTheDocument();
  });
});

describe("AddIntegrationDialog — Panel 1 (category)", () => {
  it("shows the two category cards, each with its T.CAT_* skip-if hint", () => {
    renderDialog();
    expect(screen.getByText("AI provider")).toBeInTheDocument();
    expect(screen.getByText(T.CAT_AI)).toBeInTheDocument();
    expect(screen.getByText("SCM host")).toBeInTheDocument();
    expect(screen.getByText(T.CAT_SCM)).toBeInTheDocument();
  });

  // Nothing here adds network topology any more: a proxy and an egress redirect
  // are configured on the Corporate network step, whose gate makes every
  // redirect prove "reached" — a proof this dialog could never demand.
  it("offers no Egress redirection or Host proxy card", () => {
    renderDialog();
    expect(screen.queryByText("Egress redirection")).not.toBeInTheDocument();
    expect(screen.queryByText("Host proxy")).not.toBeInTheDocument();
  });
});

describe("AddIntegrationDialog — Panel 2, AI type pick", () => {
  it("selecting AWS Bedrock reveals its four-lane sub-choice, in precedence order, with T.LANE_SWITCH", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.click(screen.getByRole("button", { name: /^continue$/i })); // AI provider is preselected
    await user.click(screen.getByText("AWS Bedrock"));

    expect(screen.getByText(/^Bearer token/)).toBeInTheDocument();
    expect(screen.getByText(/^AWS SSO/)).toBeInTheDocument();
    expect(screen.getByText(/^Host ~\/\.aws profile/)).toBeInTheDocument();
    expect(screen.getByText(/^Access keys/)).toBeInTheDocument();
    expect(screen.getByText(T.LANE_SWITCH, { exact: false })).toBeInTheDocument();
  });

  it("carries the picked lane into Panel 3 (Access keys -> three key rows, none of which is a login pane)", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.click(screen.getByRole("button", { name: /^continue$/i }));
    await user.click(screen.getByText("AWS Bedrock"));
    await user.click(screen.getByText(/^Access keys/));
    await user.click(screen.getByRole("button", { name: /^continue$/i }));

    expect(await screen.findByText("aws-access-key-id")).toBeInTheDocument();
    expect(screen.getByText("aws-secret-access-key")).toBeInTheDocument();
    expect(screen.getByText("aws-session-token")).toBeInTheDocument();
    expect(screen.queryByTestId("harness-login-pane")).toBeNull();
  });
});

describe("AddIntegrationDialog — Panel 3, Azure's features-only framing", () => {
  it("Claude Code/Codex collapse into one impossible fact row with NO switch; only Wardyn features is a real ON row", async () => {
    const user = userEvent.setup();
    renderDialog();
    await user.click(screen.getByRole("button", { name: /^continue$/i }));
    await user.click(screen.getByText("Azure OpenAI"));
    await user.click(screen.getByRole("button", { name: /^continue$/i }));

    expect(await screen.findByText("Claude Code · Codex CLI")).toBeInTheDocument();
    expect(screen.getByTitle(T.X_AZURE_HARNESS)).toBeInTheDocument();
    expect(screen.getByTitle(T.X_AZURE_DIRECT)).toBeInTheDocument();
    expect(screen.getByText("Wardyn features")).toBeInTheDocument();

    // Exactly one live control on the whole capability table — the Wardyn
    // features row. The two impossible rows get text, never a control.
    expect(screen.getAllByRole("switch")).toHaveLength(1);

    // Azure's residency is the mock's 4th kind — distinct from "varies".
    expect(screen.getByText("control-plane side")).toBeInTheDocument();
  });

  it("previews a 'replaces <name>' note when an existing integration already holds that default", async () => {
    const user = userEvent.setup();
    const existing: IntegrationRow = {
      id: "ai:anthropic_api_key",
      category: "ai_provider",
      name: "Team API key",
      typeLabel: "anthropic · api key",
      chips: [{ label: "Wardyn features · default", tone: "info" }],
      residency: "proxy_injected",
      posture: { kind: "configured" },
      secretNames: ["anthropic-api-key"],
      checkIds: [],
    };
    renderDialog([existing]);
    await user.click(screen.getByRole("button", { name: /^continue$/i }));
    await user.click(screen.getByText("Azure OpenAI"));
    await user.click(screen.getByRole("button", { name: /^continue$/i }));

    expect(await screen.findByText("Default for Wardyn features")).toBeInTheDocument();
    expect(screen.getByText(/replaces Team API key/)).toBeInTheDocument();
  });
});
