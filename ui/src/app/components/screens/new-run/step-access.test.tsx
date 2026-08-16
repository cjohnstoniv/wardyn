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
import { StepAccess, ModelAccessCard } from "./step-access";
import { initialWizardState } from "./wizard-types";
import { T } from "../../../lib/integrations";
import { RD } from "../../../lib/workspace-copy";
import { baseStatus } from "../../../lib/test-fixtures";
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
    baseStatus({ integrations: [{ id: "anthropic_api_key", kind: "anthropic_api_key", default_for: ["agent_runs"] }] }),
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
  serverId: "openai_api_key",
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

// The azureFeaturesOnly fixture lived here: a row impossible for BOTH agents,
// proving such a row gets the amber none-line rather than the neutral
// wait-for-review one. azure_openai was the only kind that could ever be that
// row, and it was removed in 0.5 — see couldDriveSomeAgent in step-access.tsx,
// which is now a guard with no reachable false branch.

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
    expect(await screen.findByText(RD.NONE_LINE("Claude Code"))).toBeInTheDocument();
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

  // The server's tier-3 resolves only a row actually marked
  // DefaultFor:agent_runs — the first COMPATIBLE row in list order is not
  // enough, even though it's the only configured integration.
  it("a compatible row that is NOT genuinely marked default falls through to the honest none-line", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([teamKey]));
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ integrations: [{ id: "anthropic_api_key", kind: "anthropic_api_key" }] }),
    );
    renderStep();
    expect(await screen.findByText(RD.NONE_LINE("Claude Code"))).toBeInTheDocument();
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
        integrations: [{ id: "anthropic_subscription:managed", kind: "anthropic_subscription" }],
      }),
    );
    renderStep();
    expect(await screen.findByText("Claude subscription (managed)")).toBeInTheDocument();
    expect(screen.getByText(/Applies because: the server's global provider config applies\./)).toBeInTheDocument();
    expect(screen.queryByText(RD.NONE_LINE("Claude Code"))).not.toBeInTheDocument();
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
        integrations: [{ id: "bedrock", kind: "bedrock" }],
        // Region set, nothing else — configured() (the row exists) is true,
        // but ready() (region AND model AND a credential) is false.
        bedrock: { region: "us-east-1", creds_present: false, ready: false },
      }),
    );
    renderStep();
    expect(await screen.findByText(RD.NONE_LINE("Claude Code"))).toBeInTheDocument();
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
        integrations: [{ id: "bedrock", kind: "bedrock" }],
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
    expect(screen.queryByText(RD.NONE_LINE("Claude Code"))).not.toBeInTheDocument();
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
        integrations: [{ id: "bedrock", kind: "bedrock" }],
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
    expect(screen.queryByText(RD.NONE_LINE("Claude Code"))).not.toBeInTheDocument();
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
          { id: "anthropic_subscription:managed", kind: "anthropic_subscription" },
          { id: "bedrock", kind: "bedrock" },
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

  // W15-W15e-wizard-roundtrip-2: the peek must commit row.serverId — the id
  // create/preflight actually resolve (resolveIntegrationRef) — never
  // row.id, the client display namespace ("ai:…"/"int-…") that both 400.
  it("committing an override patches integrationId with the row's SERVER id, not the client display id", async () => {
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

    // teamKey.id is "int-team-key" (the display id) — teamKey.serverId is
    // "anthropic_api_key" (what the server actually resolves).
    expect(patched).toEqual({ integrationId: "anthropic_api_key" });
  });

  // Same defect, exercised against a REAL deriveAiRows-shaped row (the
  // "ai:…" id namespace + aiServerId's actual mapping), not the synthetic
  // "int-team-key"/"anthropic_api_key" fixture above — the id/serverId gap
  // this finding is about is invisible unless the id namespaces actually
  // differ the way deriveAiRows really produces them.
  it("a real deriveAiRows row commits its aiServerId, and a row with no serverId can't be picked at all", async () => {
    const realApiKeyRow: IntegrationRow = {
      id: "ai:anthropic_api_key",
      serverId: "anthropic_api_key",
      category: "ai_provider",
      name: "Anthropic (API key)",
      typeLabel: "anthropic · api key",
      chips: [],
      residency: "proxy_injected",
      posture: { kind: "configured" },
      secretNames: ["anthropic-api-key"],
      aiType: "anthropic_api_key",
      checkIds: [],
    };
    // ai:anthropic_cli_login: a passive CLI-login detection with NO serverId
    // (see integrations.ts) — nothing server-side to adopt/commit.
    const cliLoginDetected: IntegrationRow = {
      id: "ai:anthropic_cli_login",
      category: "ai_provider",
      name: "Claude Code CLI (resident login)",
      typeLabel: "anthropic · cli login detected",
      chips: [],
      residency: "proxy_injected",
      posture: { kind: "configured" },
      secretNames: [],
      aiType: "anthropic_subscription",
      hostCli: true,
      checkIds: [],
    };
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([realApiKeyRow, cliLoginDetected]));
    let patched: Record<string, unknown> | null = null;
    const user = userEvent.setup();
    renderStep({ patch: (p) => { patched = p; } });

    await screen.findByText("Anthropic (API key)");
    await user.click(screen.getByRole("button", { name: /override for this run/i }));
    const peek = await screen.findByRole("dialog");

    // No-serverId row is skipped entirely — never rendered as an option.
    expect(within(peek).queryByText("Claude Code CLI (resident login)")).toBeNull();

    await user.click(within(peek).getByText("Anthropic (API key)"));
    await user.click(within(peek).getByRole("button", { name: /use this integration/i }));

    expect(patched).toEqual({ integrationId: "anthropic_api_key" });
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

// M5/M7/M8: ModelAccessCard's first-paint loading gate and its two variants
// when rendered with no onPatch (the compose-form usage, before a proposal
// names a real agent) — driven directly (not through StepAccess, which always
// supplies onPatch and so never exercises this branch).
describe("ModelAccessCard — loading gate and no-onPatch (compose-form) variants", () => {
  it("shows a neutral resolving line before all three self-fetches settle", () => {
    // Never-resolving promises: assert the very FIRST paint, before any
    // effect's .then/.finally has had a chance to run.
    listWorkspacesMock.mockReturnValue(new Promise(() => {}));
    listIntegrationsMock.mockReturnValue(new Promise(() => {}));
    getSetupStatusMock.mockReturnValue(new Promise(() => {}));
    render(<ModelAccessCard agent="claude-code" primaryWorkspaceId={undefined} />);
    expect(screen.getByText(RD.RESOLVING_LINE)).toBeInTheDocument();
    expect(screen.queryByText(RD.NONE_LINE("Claude Code"))).toBeNull();
  });

  it("renders the neutral agent-mismatch line (not amber) when integrations exist, onPatch is absent, and none resolve", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([teamKey]));
    // Configured, but not marked DefaultFor:agent_runs -> nothing resolves.
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ integrations: [{ id: "anthropic_api_key", kind: "anthropic_api_key" }] }),
    );
    render(<ModelAccessCard agent="claude-code" primaryWorkspaceId={undefined} />);
    expect(await screen.findByText(RD.AGENT_AT_REVIEW_LINE)).toBeInTheDocument();
    expect(screen.queryByText(RD.NONE_LINE("Claude Code"))).toBeNull();
  });

  it("keeps the amber zero-providers line when onPatch is absent and there are genuinely no ai_provider integrations", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([]));
    getSetupStatusMock.mockResolvedValue(baseStatus());
    render(<ModelAccessCard agent="claude-code" primaryWorkspaceId={undefined} />);
    expect(await screen.findByText(RD.NONE_LINE("Claude Code"))).toBeInTheDocument();
  });

  // The genuine agent-mismatch case stays neutral: an OpenAI key can't drive
  // Claude Code (the display agent here) but genuinely could drive Codex CLI —
  // a real "wait for review" case, unlike the Azure-only case above.
  it("keeps the neutral wait-for-review line when the only integration could drive a DIFFERENT agent (codex, not the display agent)", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([openaiTeam]));
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ integrations: [{ id: "openai_api_key", kind: "openai_api_key" }] }),
    );
    render(<ModelAccessCard agent="claude-code" primaryWorkspaceId={undefined} />);
    expect(await screen.findByText(RD.AGENT_AT_REVIEW_LINE)).toBeInTheDocument();
    expect(screen.queryByText(RD.NONE_LINE("Claude Code"))).toBeNull();
  });

  // LOW test blind spot: onPatch absent renders the workspace link (never the
  // override button/peek), and its href degrades to the plain /workspaces list
  // when there is no primary workspace to link to.
  it("renders 'Change on the workspace' (never the override button) when onPatch is absent, degrading to /workspaces with no primary", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([]));
    getSetupStatusMock.mockResolvedValue(baseStatus());
    render(<ModelAccessCard agent="claude-code" primaryWorkspaceId={undefined} />);
    await screen.findByText(RD.NONE_LINE("Claude Code"));
    const link = screen.getByRole("link", { name: /change on the workspace/i });
    expect(link).toHaveAttribute("href", "/workspaces");
    expect(link).toHaveAttribute("target", "_blank");
    expect(screen.queryByRole("button", { name: /override for this run/i })).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("links straight to the primary workspace's page when one is set", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    listIntegrationsMock.mockResolvedValue(integrations([]));
    getSetupStatusMock.mockResolvedValue(baseStatus());
    render(<ModelAccessCard agent="claude-code" primaryWorkspaceId="ws-1" />);
    await screen.findByText(RD.NONE_LINE("Claude Code"));
    expect(screen.getByRole("link", { name: /change on the workspace/i })).toHaveAttribute("href", "/workspaces/ws-1");
  });

  // W15-W15e-wizard-roundtrip-5: a workspace pin (llm_cred.integration_ref)
  // that doesn't resolve to any fetched integration row must refuse here —
  // mirroring the server's resolveRunIntegration (internal/api/llmcred.go),
  // which returns "no binding" the instant a SET workspace ref fails to
  // resolve, rather than falling through to the server-default tier. Before
  // the fix this cascaded past the dangling pin and resolved (and rendered)
  // a DIFFERENT provider — one the actual launch would never use.
  it("a workspace pinned to an integration that no longer resolves shows the honest none-line, not a different provider", async () => {
    listWorkspacesMock.mockResolvedValue([
      {
        id: "ws-1",
        name: "payments",
        kind: "repo",
        source: "acme/payments",
        status: "scanned",
        created_at: "now",
        updated_at: "now",
        llm_cred: { integration_ref: "int-deleted" },
      },
    ]);
    // teamKey IS the genuinely-marked server default — must NOT be shown,
    // even though it would resolve fine on its own.
    listIntegrationsMock.mockResolvedValue(integrations([teamKey]));
    // onPatch present (the manual wizard's usage) so an unresolved case
    // renders the amber none-line rather than the compose-form's softer
    // neutral "resolves at review" line (M7) — this is the wizard's own path.
    render(<ModelAccessCard agent="claude-code" primaryWorkspaceId="ws-1" onPatch={() => {}} />);
    expect(await screen.findByText(RD.NONE_LINE("Claude Code"))).toBeInTheDocument();
    expect(screen.queryByText("Team API key")).not.toBeInTheDocument();
    expect(screen.queryByText(/this workspace pins it/i)).not.toBeInTheDocument();
  });
});
