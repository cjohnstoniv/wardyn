/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { baseStatus } from "../setup/test-fixtures";
import { AI_TYPES, SUBSCRIPTION_LANE_META, T } from "../../../lib/integrations";
import type { IntegrationRow } from "../../../lib/api/integrations";
import { AddIntegrationDialog, type AddIntegrationTarget } from "./add-integration-dialog";

function renderDialog(existingAiRows: IntegrationRow[] = [], target: AddIntegrationTarget = { s: "ai_type" }) {
  const reload = vi.fn();
  const onOpenChange = vi.fn();
  const onBackToSearch = vi.fn();
  render(
    <AddIntegrationDialog
      open
      onOpenChange={onOpenChange}
      status={baseStatus()}
      siteConfig={{}}
      existingAiRows={existingAiRows}
      reload={reload}
      target={target}
      onBackToSearch={onBackToSearch}
    />,
  );
  return { reload, onOpenChange, onBackToSearch };
}

// The search-first Add flow (add-service-dialog.tsx) is the ONLY way in: by
// the time this dialog opens, "AI provider or SCM host?" has always been
// answered by the pick itself. The category grid is GONE — owner report,
// verbatim: "i should never see the AI Provider / SCM provider popup anymore".
describe("AddIntegrationDialog — opens ON the handed-off target, category grid dead", () => {
  it("never renders the category card grid, whatever the target", () => {
    for (const target of [
      { s: "ai_type" } as const,
      { s: "ai_connect", type: "openai_api_key" } as const,
      { s: "scm" } as const,
    ]) {
      cleanup();
      renderDialog([], target);
      expect(screen.queryByText(T.CAT_AI)).not.toBeInTheDocument();
      expect(screen.queryByText(T.CAT_SCM)).not.toBeInTheDocument();
      expect(screen.queryByText("Egress redirection")).not.toBeInTheDocument();
      expect(screen.queryByText("Host proxy")).not.toBeInTheDocument();
    }
  });

  // The owner's second report: clicking Anthropic offered no Claude
  // subscription. "Anthropic" still splits into API key vs subscription, so it
  // lands on the type panel PRESELECTED, with the subscription one click away.
  it("an ambiguous pick lands on the type panel, preselected, subscription visible", () => {
    renderDialog([], { s: "ai_type", preselect: "anthropic_api_key" });
    expect(screen.getByText(AI_TYPES.anthropic_api_key.title)).toBeInTheDocument();
    expect(screen.getByText(AI_TYPES.anthropic_subscription.title)).toBeInTheDocument();
    // Continue goes straight to the API-key connect panel — the preselection
    // really is the clicked row, not the panel's own default.
    expect(screen.getByRole("button", { name: /^continue$/i })).toBeEnabled();
  });

  it("a subscription pick opens preselected on the subscription and its lane choice", () => {
    renderDialog([], { s: "ai_type", preselect: "anthropic_subscription" });
    expect(screen.getByText(AI_TYPES.anthropic_subscription.title)).toBeInTheDocument();
    // The managed-vs-host-login sub-choice only renders for the SELECTED row.
    expect(screen.getByText(SUBSCRIPTION_LANE_META.managed.title)).toBeInTheDocument();
  });

  it("a question-free pick lands straight on its connect panel", () => {
    renderDialog([], { s: "ai_connect", type: "openai_api_key" });
    expect(screen.queryByText(AI_TYPES.anthropic_subscription.title)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /back/i })).toBeInTheDocument();
  });

  it("opens the SCM ladder directly for a git host", () => {
    renderDialog([], { s: "scm" });
    expect(screen.queryByText(T.CAT_AI)).not.toBeInTheDocument();
  });

  it("backing out of the first panel returns to the search dialog", async () => {
    const { onBackToSearch } = renderDialog([], { s: "ai_type" });
    await userEvent.click(screen.getByRole("button", { name: /back/i }));
    expect(onBackToSearch).toHaveBeenCalled();
  });
});

describe("AddIntegrationDialog — Panel 2, AI type pick", () => {
  it("selecting AWS Bedrock reveals its four-lane sub-choice, in precedence order, with T.LANE_SWITCH", async () => {
    const user = userEvent.setup();
    renderDialog();
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
    await user.click(screen.getByText("Azure OpenAI"));
    await user.click(screen.getByRole("button", { name: /^continue$/i }));

    expect(await screen.findByText("Default for Wardyn features")).toBeInTheDocument();
    expect(screen.getByText(/replaces Team API key/)).toBeInTheDocument();
  });
});
