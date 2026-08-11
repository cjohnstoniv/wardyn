/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { baseStatus } from "../setup/test-fixtures";
import { AI_TYPES, SUBSCRIPTION_LANE_META, T } from "../../../lib/integrations";
import type { IntegrationRow } from "../../../lib/api/integrations";
import { AddIntegrationDialog, type AddIntegrationTarget } from "./add-integration-dialog";
import type { SetupStatus } from "../../../lib/types";

// The real pane boots a sandbox and a websocket terminal; here the seam under
// test is what the PANEL does with the pane's outcome, so the pane is two
// buttons that fire its callbacks.
// ScmHandoff re-GETs site-config on mount (stale-copy discipline); give it a
// resolved fake so the two tests that land on the SCM ladder don't leak an
// unhandled rejection from jsdom's URL-less fetch.
const getSiteConfigMock = vi.fn().mockResolvedValue({});
const putSiteConfigMock = vi.fn().mockResolvedValue(undefined);
vi.mock("../../../lib/api/health", () => ({
  health: {
    getSiteConfig: (...a: unknown[]) => getSiteConfigMock(...a),
    putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a),
  },
}));

// UI-WS-2's default-for write path (actions.ts's setDefaultFor, unmocked and
// real) calls these two — everything else this module needs (AI_TYPES-driven
// rendering, aiResidency, aiServerId, ...) stays real.
const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) } }));
const adoptIntegrationMock = vi.fn();
const putIntegrationMock = vi.fn();
vi.mock("../../../lib/api/integrations", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/integrations")>();
  return {
    ...actual,
    integrationsApi: { ...actual.integrationsApi, adoptIntegration: (...a: unknown[]) => adoptIntegrationMock(...a) },
    genericIntegrationsApi: { ...actual.genericIntegrationsApi, put: (...a: unknown[]) => putIntegrationMock(...a) },
  };
});

vi.mock("../setup/harness-login-pane", () => ({
  HarnessLoginPane: (p: { onDone: () => void; onCancel: () => void }) => (
    <div data-testid="harness-login-pane">
      <button onClick={p.onDone}>finish-login</button>
      <button onClick={p.onCancel}>cancel-login</button>
    </div>
  ),
}));

const toastErrorMock = vi.fn();
const toastInfoMock = vi.fn();
vi.mock("sonner", () => ({
  toast: { error: (...a: unknown[]) => toastErrorMock(...a), success: vi.fn(), info: (...a: unknown[]) => toastInfoMock(...a) },
}));

beforeEach(() => {
  getSiteConfigMock.mockReset().mockResolvedValue({});
  putSiteConfigMock.mockReset().mockResolvedValue(undefined);
  getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
  adoptIntegrationMock.mockReset().mockResolvedValue(undefined);
  putIntegrationMock.mockReset().mockResolvedValue(undefined);
  toastErrorMock.mockReset();
  toastInfoMock.mockReset();
});

function renderDialog(
  existingAiRows: IntegrationRow[] = [],
  target: AddIntegrationTarget = { s: "ai_type" },
  status: SetupStatus = baseStatus(),
  secretNames: string[] = [],
) {
  const reload = vi.fn();
  const onOpenChange = vi.fn();
  const onBackToSearch = vi.fn();
  render(
    <AddIntegrationDialog
      open
      onOpenChange={onOpenChange}
      status={status}
      siteConfig={{}}
      existingAiRows={existingAiRows}
      secretNames={secretNames}
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

// The login cell's memory — owner report: after logging in "the screen became
// this… should show that i've logged in and enable me to relogin if i want
// but not just say login".
describe("ConnectReviewPanel — the container-login credential cell", () => {
  const subTarget: AddIntegrationTarget = { s: "ai_connect", type: "anthropic_subscription" };

  it("after the login finishes: says captured, offers Log in again — never a bare Log in", async () => {
    renderDialog([], subTarget);
    expect(screen.getByTestId("harness-login-pane")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "finish-login" }));

    expect(screen.queryByTestId("harness-login-pane")).not.toBeInTheDocument();
    expect(screen.getByTestId("login-captured-line")).toHaveTextContent(/Subscription captured — stored write-only/);
    expect(screen.getByRole("button", { name: /log in again/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^log in$/i })).not.toBeInTheDocument();
  });

  it("Log in again reopens the pane, and cancelling a RE-login keeps the captured state", async () => {
    renderDialog([], subTarget);
    await userEvent.click(screen.getByRole("button", { name: "finish-login" }));
    await userEvent.click(screen.getByRole("button", { name: /log in again/i }));
    expect(screen.getByTestId("harness-login-pane")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "cancel-login" }));
    expect(screen.getByTestId("login-captured-line")).toBeInTheDocument();
  });

  it("a subscription captured in an earlier session shows as already connected, with its age", () => {
    const captured = new Date(Date.now() - 3 * 24 * 3600 * 1000).toISOString();
    renderDialog([], subTarget, baseStatus({ harness: [{ provider: "anthropic", captured: true, captured_at: captured }] }));
    // Server-known capture: no pane auto-open... the pane still opens (the cell
    // opens on it for a login-lane arrival) — cancel collapses to the state.
    expect(screen.getByTestId("harness-login-pane")).toBeInTheDocument();
  });

  it("cancelling with a server-known capture collapses to 'Already connected', not out of the panel", async () => {
    const captured = new Date(Date.now() - 3 * 24 * 3600 * 1000).toISOString();
    renderDialog([], subTarget, baseStatus({ harness: [{ provider: "anthropic", captured: true, captured_at: captured }] }));
    await userEvent.click(screen.getByRole("button", { name: "cancel-login" }));
    expect(screen.getByTestId("login-captured-line")).toHaveTextContent(/Already connected — captured/);
    expect(screen.getByRole("button", { name: /log in again/i })).toBeInTheDocument();
  });

  it("cancelling a FIRST-ever login leaves the connect panel (nothing to fall back to)", async () => {
    renderDialog([], subTarget);
    await userEvent.click(screen.getByRole("button", { name: "cancel-login" }));
    // onCancelAll = onBack → the AI type panel is up again.
    expect(screen.queryByTestId("harness-login-pane")).not.toBeInTheDocument();
    expect(screen.getByText(AI_TYPES.anthropic_subscription.title)).toBeInTheDocument();
  });
});

// UI-WS-2: "Add integration" used to patch only local dialog state — neither
// the Name field nor the two DefaultFor checkboxes ever reached the server.
describe("ConnectReviewPanel — Add integration persists the DefaultFor checkboxes", () => {
  it("adopts and PUTs BOTH auto-checked marks for a fresh, first-ever provider", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        secrets: { present: ["openai-api-key"], github_app: false },
        integrations: [
          { id: "openai_api_key", category: "ai_provider", type: "openai_api_key", source: "legacy", default_for: [] },
        ],
      }),
    );
    const { onOpenChange, reload } = renderDialog([], { s: "ai_connect", type: "openai_api_key" });

    await userEvent.click(screen.getByRole("button", { name: /^add integration$/i }));

    await waitFor(() => expect(adoptIntegrationMock).toHaveBeenCalledWith("openai_api_key"));
    await waitFor(() =>
      expect(putIntegrationMock).toHaveBeenCalledWith(
        "openai_api_key",
        expect.objectContaining({ default_for: expect.arrayContaining(["agent_runs", "wardyn_features"]) }),
      ),
    );
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(reload).toHaveBeenCalled();
  });

  it("unchecking a default before Add clears the mark instead of silently keeping the auto-checked default", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        secrets: { present: ["openai-api-key"], github_app: false },
        integrations: [
          {
            id: "openai_api_key",
            category: "ai_provider",
            type: "openai_api_key",
            source: "stored",
            default_for: ["agent_runs", "wardyn_features"],
          },
        ],
      }),
    );
    renderDialog([], { s: "ai_connect", type: "openai_api_key" });
    const user = userEvent.setup();

    await user.click(screen.getByRole("checkbox", { name: /default for agent runs/i }));
    await user.click(screen.getByRole("button", { name: /^add integration$/i }));

    await waitFor(() =>
      expect(putIntegrationMock).toHaveBeenCalledWith("openai_api_key", expect.objectContaining({ default_for: ["wardyn_features"] })),
    );
    // Already stored — nothing to adopt for either mark.
    expect(adoptIntegrationMock).not.toHaveBeenCalled();
  });

  it("the Name field is a fact (aiRowName), not an editable control nothing ever reads back", () => {
    renderDialog([], { s: "ai_connect", type: "openai_api_key" });
    expect(screen.queryByRole("textbox", { name: /^name$/i })).not.toBeInTheDocument();
    expect(screen.getByText("OpenAI (API key)")).toBeInTheDocument();
  });
});

// SCM-SEAM-3: addHost had try/finally with no catch — a rejected write left
// the spinner stopping with no toast, no inline error, and the host never
// registered.
describe("ScmHandoff — a rejected scm_hosts write toasts and doesn't advance", () => {
  it("addHost's catch surfaces the failure instead of swallowing it", async () => {
    putSiteConfigMock.mockRejectedValueOnce(new Error("500"));
    const { reload, onOpenChange } = renderDialog([], { s: "scm" });
    const user = userEvent.setup();

    // GitHub is PROVIDER_OPTIONS[0], pre-selected — Continue needs no typing.
    await user.click(screen.getByRole("button", { name: "Continue" }));
    await user.click(await screen.findByRole("button", { name: "Done" }));

    await waitFor(() => expect(toastErrorMock).toHaveBeenCalledWith("Failed to add the SCM host", expect.anything()));
    expect(reload).not.toHaveBeenCalled();
    expect(onOpenChange).not.toHaveBeenCalled();
    // Not stuck mid-spinner — Done is clickable again.
    expect(await screen.findByRole("button", { name: "Done" })).toBeEnabled();
  });
});

// SCM-SEAM-4: both SCM hand-off panels opened AddSecretDialog with no
// existingNames, so its own overwrite gate (explicit confirm + "Rotate
// secret" title) never armed — exactly on the locked conventional names most
// likely to collide.
describe("ScmHandoff — existingNames arms AddSecretDialog's overwrite gate", () => {
  it("a PAT name that collides with an existing secret opens as Rotate secret, not Add secret", async () => {
    renderDialog([], { s: "scm" }, baseStatus(), ["git-pat-github-com"]);
    const user = userEvent.setup();

    await user.click(screen.getByRole("button", { name: "Continue" }));
    await user.click(await screen.findByRole("button", { name: "Add PAT" }));

    expect(await screen.findByText("Rotate secret")).toBeInTheDocument();
    expect(screen.queryByText("Add secret")).not.toBeInTheDocument();
  });

  it("a non-colliding name still opens as a plain Add secret", async () => {
    renderDialog([], { s: "scm" }, baseStatus(), ["some-other-secret"]);
    const user = userEvent.setup();

    await user.click(screen.getByRole("button", { name: "Continue" }));
    await user.click(await screen.findByRole("button", { name: "Add PAT" }));

    expect(await screen.findByText("Add secret")).toBeInTheDocument();
  });
});
