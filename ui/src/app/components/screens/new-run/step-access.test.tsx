/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// StepAccess's model-access card must RESOLVE from integrations rather than
// take a manual per-run auth choice: a resolved integration (name + type +
// residency chip + "Applies because"), the honest amber RD.NONE_LINE when
// nothing resolves, and — for a governed command — no card at all, just
// RD.EXEC_LINE. The "Override for this run…" peek must mute an incompatible
// integration with its verbatim IMPOSSIBLE-map reason and never let it be
// picked.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { StepAccess } from "./step-access";
import { initialWizardState } from "./wizard-types";
import { T } from "../../../lib/integrations";
import { RD } from "../../../lib/workspace-copy";
import { baseStatus } from "../setup/test-fixtures";
import type { IntegrationRow } from "../../../lib/api/integrations";

const listIntegrationsMock = vi.fn();
vi.mock("../../../lib/api/integrations", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/integrations")>();
  return { ...actual, integrationsApi: { list: (...a: unknown[]) => listIntegrationsMock(...a) } };
});

const listWorkspacesMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a) },
}));

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

// Module-level (not per-describe) so every block shares one reset — these two
// mocks are call-count-sensitive in the "never fetches for a governed command"
// case, and vitest does not clear mocks between tests by default.
beforeEach(() => {
  listIntegrationsMock.mockReset();
  listWorkspacesMock.mockReset();
  getSetupStatusMock.mockReset();
  // Default: teamKey below IS the genuinely-marked server default — matches
  // the "server default" wording pinned throughout this file. Tests that care
  // about the unmarked case override this per-test.
  getSetupStatusMock.mockResolvedValue(
    baseStatus({ integrations: [{ id: "anthropic_api_key", category: "ai_provider", type: "anthropic_api_key", default_for: ["agent_runs"] }] }),
  );
});

function integrations(ai: IntegrationRow[]) {
  return { ai, scm: [], mirror: [], proxy: [] };
}

const teamKey: IntegrationRow = {
  id: "int-team-key",
  serverId: "anthropic_api_key",
  category: "ai_provider",
  name: "Team API key",
  typeLabel: "anthropic · api key",
  chips: [],
  residency: "proxy_injected",
  posture: { kind: "configured" },
  secretNames: ["anthropic-api-key"],
  aiType: "anthropic_api_key",
  checkIds: [],
};

const openaiTeam: IntegrationRow = {
  id: "int-openai",
  category: "ai_provider",
  name: "OpenAI (team)",
  typeLabel: "openai · api key",
  chips: [],
  residency: "proxy_injected",
  posture: { kind: "configured" },
  secretNames: ["openai-api-key"],
  aiType: "openai_api_key",
  checkIds: [],
};

function renderStep(overrides?: Partial<Parameters<typeof StepAccess>[0]>) {
  return render(
    <StepAccess
      state={initialWizardState()}
      patch={() => {}}
      secrets={[]}
      secretsLoading={false}
      onAddSecret={() => {}}
      {...overrides}
    />,
  );
}

describe("StepAccess — model access resolution card", () => {
  it("shows the resolved integration with its type, residency chip, and why it applies", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([teamKey]));
    renderStep();
    expect(await screen.findByText("Team API key")).toBeInTheDocument();
    expect(screen.getByText("anthropic · api key")).toBeInTheDocument();
    expect(screen.getByText("proxy-injected")).toBeInTheDocument();
    expect(screen.getByText(/Applies because: it's the server default for agent runs\./)).toBeInTheDocument();
    expect(screen.getByText("Model access — resolved from integrations")).toBeInTheDocument();
  });

  it("shows the honest amber line when nothing resolves", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([])); // nothing configured
    renderStep();
    expect(await screen.findByText(RD.NONE_LINE)).toBeInTheDocument();
  });

  it("is absent entirely for a governed command, showing RD.EXEC_LINE instead", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([teamKey]));
    renderStep({ state: { ...initialWizardState(), runType: "command" } });
    expect(screen.getByText(RD.EXEC_LINE)).toBeInTheDocument();
    expect(screen.queryByText("Model access — resolved from integrations")).toBeNull();
    expect(screen.queryByRole("button", { name: /override for this run/i })).toBeNull();
    // GitHub token card is untouched by the governed-command case.
    expect(screen.getByText("GitHub token")).toBeInTheDocument();
  });

  it("never fetches integrations at all for a governed command", () => {
    listWorkspacesMock.mockResolvedValue([]);
    renderStep({ state: { ...initialWizardState(), runType: "command" } });
    expect(listIntegrationsMock).not.toHaveBeenCalled();
    expect(getSetupStatusMock).not.toHaveBeenCalled();
  });

  // The server's tier-4 resolves only a row actually marked
  // DefaultFor:agent_runs — the first COMPATIBLE row in list order is not
  // enough, even though it's the only configured integration.
  it("a compatible row that is NOT genuinely marked default falls through to the honest none-line", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([teamKey]));
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ integrations: [{ id: "anthropic_api_key", category: "ai_provider", type: "anthropic_api_key" }] }),
    );
    renderStep();
    expect(await screen.findByText(RD.NONE_LINE)).toBeInTheDocument();
    expect(screen.queryByText("Team API key")).not.toBeInTheDocument();
  });

  // ok=false at the DefaultFor tier is NOT "no model access" — the managed-
  // subscription and global-Bedrock carve-outs credential a run with no
  // marked (or even stored) integration at all (llmcred.go's
  // resolveRunIntegration doc; preflight.go:161-179). Both show up here only
  // as the two fixed-id derived rows, never an ordinary unmarked row (pinned
  // above), so this can't regress back into the pre-item-8 "guess the first
  // row" defect.
  it("an unmarked install still resolves via the managed-subscription global carve-out, not the honest-none warning", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    const managedSubscription: IntegrationRow = {
      id: "ai:anthropic_subscription:managed",
      serverId: "anthropic_subscription:managed",
      category: "ai_provider",
      name: "Claude subscription (managed)",
      typeLabel: "anthropic · subscription",
      chips: [],
      residency: "proxy_injected",
      posture: { kind: "configured" },
      secretNames: [],
      aiType: "anthropic_subscription",
      checkIds: [],
    };
    listIntegrationsMock.mockResolvedValue(integrations([managedSubscription]));
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        integrations: [{ id: "anthropic_subscription:managed", category: "ai_provider", type: "anthropic_subscription" }],
      }),
    );
    renderStep();
    expect(await screen.findByText("Claude subscription (managed)")).toBeInTheDocument();
    expect(screen.getByText(/Applies because: the server's global provider config applies\./)).toBeInTheDocument();
    expect(screen.queryByText(RD.NONE_LINE)).not.toBeInTheDocument();
  });

  // The derived "bedrock" row exists whenever the operator has touched ANY
  // Bedrock knob (SetupBedrock.configured), which is weaker than readiness
  // (SetupBedrock.ready — region+model+a credential). The carve-out must gate
  // on the latter (status.bedrock.ready) or the wizard names a provider whose
  // first model call 404s — and, because a truthy `resolved` also suppresses
  // step-review.tsx's "No model access" warning, hides the very problem it
  // causes. Both directions pinned below.
  it("a touched-but-not-ready Bedrock config yields no model access, not the carve-out", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    const bedrock: IntegrationRow = {
      id: "ai:bedrock",
      serverId: "bedrock",
      category: "ai_provider",
      name: "AWS Bedrock",
      typeLabel: "bedrock",
      chips: [],
      residency: "proxy_injected",
      posture: { kind: "configured" },
      secretNames: [],
      aiType: "bedrock",
      checkIds: [],
    };
    listIntegrationsMock.mockResolvedValue(integrations([bedrock]));
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        integrations: [{ id: "bedrock", category: "ai_provider", type: "bedrock" }],
        // Region set, nothing else — configured() (the row exists) is true,
        // but ready() (region AND model AND a credential) is false.
        bedrock: { region: "us-east-1", creds_present: false, ready: false },
      }),
    );
    renderStep();
    expect(await screen.findByText(RD.NONE_LINE)).toBeInTheDocument();
    expect(screen.queryByText("AWS Bedrock")).not.toBeInTheDocument();
  });

  it("a ready Bedrock config resolves via the global-Bedrock carve-out", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    const bedrock: IntegrationRow = {
      id: "ai:bedrock",
      serverId: "bedrock",
      category: "ai_provider",
      name: "AWS Bedrock",
      typeLabel: "bedrock",
      chips: [],
      residency: "proxy_injected",
      posture: { kind: "configured" },
      secretNames: [],
      aiType: "bedrock",
      checkIds: [],
    };
    listIntegrationsMock.mockResolvedValue(integrations([bedrock]));
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        integrations: [{ id: "bedrock", category: "ai_provider", type: "bedrock" }],
        bedrock: {
          region: "us-east-1",
          model: "anthropic.claude-3-sonnet",
          creds_present: true,
          ready: true,
        },
      }),
    );
    renderStep();
    expect(await screen.findByText("AWS Bedrock")).toBeInTheDocument();
    expect(screen.queryByText(RD.NONE_LINE)).not.toBeInTheDocument();
  });

  // Readiness is the SERVER's verdict, and it counts every credential lane
  // resolveBedrockAuth accepts — including a captured container-login AWS SSO
  // session with NO host ~/.aws mount and NO static keys, which is exactly the
  // configuration harness-login-pane advertises. While ready() was narrower than
  // the launch gate, this operator's run authenticated fine and Access still
  // said nothing could drive Claude Code.
  it("a Bedrock config ready only via a captured AWS SSO session resolves via the carve-out", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    const bedrock: IntegrationRow = {
      id: "ai:bedrock",
      serverId: "bedrock",
      category: "ai_provider",
      name: "AWS Bedrock",
      typeLabel: "bedrock",
      chips: [],
      residency: "resident_mount",
      posture: { kind: "session_expires", when: "14:20" },
      secretNames: [],
      harnessProvider: "aws",
      aiType: "bedrock",
      bedrockLane: "sso",
      checkIds: [],
    };
    listIntegrationsMock.mockResolvedValue(integrations([bedrock]));
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        integrations: [{ id: "bedrock", category: "ai_provider", type: "bedrock" }],
        bedrock: {
          region: "us-east-1",
          model: "anthropic.claude-3-sonnet",
          creds_present: false,
          aws_mount: false,
          bearer_present: false,
          sso_present: true,
          ready: true,
        },
        harness: [{ provider: "aws", captured: true, expired: false }],
      }),
    );
    renderStep();
    expect(await screen.findByText("AWS Bedrock")).toBeInTheDocument();
    expect(screen.queryByText(RD.NONE_LINE)).not.toBeInTheDocument();
  });

  // Dispatch injects the managed subscription only when Bedrock is NOT ready
  // (runs_dispatch_llm.go: `managed := … && !t.bedrockReady`), so an install
  // with both runs on Bedrock. Naming the subscription — which is merely what
  // deriveAiRows pushes first — also reads ITS proxy_injected residency, hiding
  // the resident-credential warning step-review renders off the named row.
  it("with BOTH a managed subscription and a ready Bedrock, names Bedrock and its resident residency — dispatch's order, not list order", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    const managedSubscription: IntegrationRow = {
      id: "ai:anthropic_subscription:managed",
      serverId: "anthropic_subscription:managed",
      category: "ai_provider",
      name: "Claude subscription (managed)",
      typeLabel: "anthropic · subscription",
      chips: [],
      residency: "proxy_injected",
      posture: { kind: "configured" },
      secretNames: [],
      aiType: "anthropic_subscription",
      checkIds: [],
    };
    const bedrock: IntegrationRow = {
      id: "ai:bedrock",
      serverId: "bedrock",
      category: "ai_provider",
      name: "AWS Bedrock",
      typeLabel: "bedrock",
      chips: [],
      residency: "resident_env",
      posture: { kind: "configured" },
      secretNames: ["aws-access-key-id", "aws-secret-access-key"],
      aiType: "bedrock",
      bedrockLane: "static",
      checkIds: [],
    };
    // deriveAiRows' own push order: managed BEFORE bedrock. Resolution must not
    // inherit it.
    listIntegrationsMock.mockResolvedValue(integrations([managedSubscription, bedrock]));
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        integrations: [
          { id: "anthropic_subscription:managed", category: "ai_provider", type: "anthropic_subscription" },
          { id: "bedrock", category: "ai_provider", type: "bedrock" },
        ],
        bedrock: { region: "us-east-1", model: "anthropic.claude-3-sonnet", creds_present: true, ready: true },
        harness: [{ provider: "anthropic", captured: true }],
      }),
    );
    renderStep();
    expect(await screen.findByText("AWS Bedrock")).toBeInTheDocument();
    expect(screen.queryByText("Claude subscription (managed)")).not.toBeInTheDocument();
    // The residency the operator is shown is Bedrock's, not the subscription's.
    expect(screen.getByText("resident")).toBeInTheDocument();
    expect(screen.queryByText("proxy-injected")).not.toBeInTheDocument();
  });
});

describe("StepAccess — Override for this run peek", () => {
  it("mutes an incompatible integration with its verbatim IMPOSSIBLE-map reason, and it can't be picked", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([teamKey, openaiTeam]));
    const user = userEvent.setup();
    renderStep(); // default agent: claude-code

    await screen.findByText("Team API key");
    await user.click(screen.getByRole("button", { name: /override for this run/i }));
    const peek = await screen.findByRole("dialog");

    // The Claude-incompatible OpenAI row is muted with the verbatim reason —
    // a fact, not a toggle — and cannot be selected.
    expect(within(peek).getByText(T.X_OPENAI_CLAUDE)).toBeInTheDocument();
    const openaiRow = within(peek).getByText("OpenAI (team)").closest("button");
    expect(openaiRow).toBeDisabled();

    // The compatible row is a real, clickable option.
    const compatibleRow = within(peek).getByText("Team API key").closest("button");
    expect(compatibleRow).not.toBeDisabled();
  });

  it("committing an override patches integrationId", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([teamKey, openaiTeam]));
    let patched: Record<string, unknown> | null = null;
    const user = userEvent.setup();
    renderStep({ patch: (p) => { patched = p; } });

    await screen.findByText("Team API key");
    await user.click(screen.getByRole("button", { name: /override for this run/i }));
    const peek = await screen.findByRole("dialog");
    await user.click(within(peek).getByText("Team API key"));
    await user.click(within(peek).getByRole("button", { name: /use this integration/i }));

    expect(patched).toEqual({ integrationId: "int-team-key" });
  });

  it("'Use the server default' clears an existing override", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([teamKey, openaiTeam]));
    let patched: Record<string, unknown> | null = null;
    const user = userEvent.setup();
    renderStep({ state: { ...initialWizardState(), integrationId: "int-openai" }, patch: (p) => { patched = p; } });

    await screen.findByText("OpenAI (team)"); // resolved: the override is honored
    await user.click(screen.getByRole("button", { name: /override for this run/i }));
    const peek = await screen.findByRole("dialog");
    await user.click(within(peek).getByText("Use the server default"));
    await user.click(within(peek).getByRole("button", { name: /use this integration/i }));

    expect(patched).toEqual({ integrationId: undefined });
  });

  it("Cancel closes the peek without changing anything", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([teamKey]));
    const patch = vi.fn();
    const user = userEvent.setup();
    renderStep({ patch });

    await screen.findByText("Team API key");
    await user.click(screen.getByRole("button", { name: /override for this run/i }));
    const peek = await screen.findByRole("dialog");
    await user.click(within(peek).getByRole("button", { name: /cancel/i }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(patch).not.toHaveBeenCalled();
  });
});
