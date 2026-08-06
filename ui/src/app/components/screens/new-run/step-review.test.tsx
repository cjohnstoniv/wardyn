/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import type { PreflightResult, Workspace } from "../../../lib/types";
import type { IntegrationRow } from "../../../lib/api/integrations";
import { StepReview } from "./step-review";
import { initialWizardState } from "./wizard-types";
import { baseStatus } from "../setup/test-fixtures";
import { RD } from "../../../lib/workspace-copy";

const listIntegrationsMock = vi.fn();
vi.mock("../../../lib/api/integrations", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/integrations")>();
  return { ...actual, integrationsApi: { list: (...a: unknown[]) => listIntegrationsMock(...a) } };
});
listIntegrationsMock.mockResolvedValue({ ai: [], scm: [], mirror: [], proxy: [] });

// resolveModelAccess's tier-3 ("server default") needs a genuinely-marked
// DefaultFor:agent_runs row (step-access.tsx) — teamKey below IS that row by
// default; the "nothing resolves" test overrides with an unmarked/empty status.
const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
getSetupStatusMock.mockResolvedValue(
  baseStatus({ integrations: [{ id: "anthropic_api_key", category: "ai_provider", type: "anthropic_api_key", default_for: ["agent_runs"] }] }),
);

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

// StepReview is a pure display component, so these render it directly with props
// (the wizard.test.tsx integration proves preflight is actually FIRED on Review).

function preflight(over: Partial<PreflightResult> = {}): PreflightResult {
  return {
    enforced_confinement_class: "CC2",
    setup_items: [
      {
        id: "backend:CC2",
        kind: "backend",
        label: "Sandbox barrier: Wall",
        required_by: "the proposal's confinement class",
        status: "satisfied",
      },
    ],
    ...over,
  };
}

describe("StepReview — preflight surfacing", () => {
  it("renders the setup checklist rows from the preflight result", () => {
    render(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={preflight()}
        preflightStatus="idle"
      />,
    );
    expect(screen.getByTestId("preflight-checklist")).toBeInTheDocument();
    expect(screen.getByTestId("setup-item-backend:CC2")).toBeInTheDocument();
  });

  it("shows a quiet 'preflight unavailable' line on error, never blocking Review", () => {
    render(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={null}
        preflightStatus="error"
      />,
    );
    expect(screen.getByTestId("preflight-unavailable")).toBeInTheDocument();
    // The inline_policy summary still renders — Review is never blocked.
    expect(screen.getByText(/inline_policy \(sent verbatim\)/i)).toBeInTheDocument();
  });

  it("renders the silent-raise line (friendly tier name) when the enforced class exceeds the pick", () => {
    render(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={preflight({ enforced_confinement_class: "CC3" })}
        preflightStatus="idle"
      />,
    );
    const line = screen.getByTestId("preflight-cc-raise");
    expect(line).toHaveTextContent(/Launches at Vault/i);
    expect(line).toHaveTextContent(/write-capable or third-party production credentials/i);
    // Never the raw wire code (e2e enforces UI copy uses Fence/Wall/Vault).
    expect(line).not.toHaveTextContent(/CC3/);
  });

  it("does NOT show the raise line when the enforced class equals the pick", () => {
    render(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={preflight({ enforced_confinement_class: "CC2" })}
        preflightStatus="idle"
      />,
    );
    expect(screen.queryByTestId("preflight-cc-raise")).toBeNull();
  });

  it("adds the autonomous-run wording to the no-model warning in batch mode", () => {
    // Default state: claude-code + apikey + no stored secret => noLlmCred.
    render(
      <StepReview
        state={{ ...initialWizardState("CC2"), mode: "batch" }}
        patch={() => {}}
        preflight={preflight()}
        preflightStatus="idle"
      />,
    );
    expect(screen.getByTestId("review-no-model-access")).toBeInTheDocument();
    expect(screen.getByTestId("review-batch-no-model")).toHaveTextContent(
      /autonomous run can't perform its task without model access/i,
    );
  });

  it("omits the autonomous wording for an interactive run", () => {
    render(
      <StepReview
        state={{ ...initialWizardState("CC2"), mode: "interactive" }}
        patch={() => {}}
        preflight={preflight()}
        preflightStatus="idle"
      />,
    );
    // The no-model warning still shows (no LLM cred), but without the batch clause.
    expect(screen.getByTestId("review-no-model-access")).toBeInTheDocument();
    expect(screen.queryByTestId("review-batch-no-model")).toBeNull();
  });
});

// A local directory used to leak through as "Repo: local:<basename>" — the
// synthetic wire label, not a fact about the workspace. The primary onboarded
// workspace is the truth: its own name + kind + source.
describe("StepReview — Workspace label (fixing the local:<basename> leak)", () => {
  function localDirWorkspace(): Workspace {
    return {
      id: "ws-1",
      name: "payments-service",
      kind: "local_dir",
      source: "/home/me/payments",
      status: "scanned",
      created_at: "",
      updated_at: "",
    } as Workspace;
  }

  it("renders a local dir as 'Workspace' with its name + kind · source, never 'Repo: local:...'", () => {
    render(
      <StepReview
        state={{ ...initialWizardState("CC2"), workspaces: [{ workspaceId: "ws-1" }] }}
        patch={() => {}}
        workspaces={[localDirWorkspace()]}
      />,
    );
    expect(screen.getByText("Workspace")).toBeInTheDocument();
    expect(screen.getByText("payments-service")).toBeInTheDocument();
    expect(screen.getByText("local dir · /home/me/payments")).toBeInTheDocument();
    expect(screen.queryByText("Repo")).toBeNull();
    expect(screen.queryByText(/local:payments-service/)).toBeNull();
  });

  it("shows 'none (ephemeral scratch)' with no workspace attached", () => {
    render(
      <StepReview state={initialWizardState("CC2")} patch={() => {}} workspaces={[]} />,
    );
    expect(screen.getByText("Workspace")).toBeInTheDocument();
    expect(screen.getByText("none (ephemeral scratch)")).toBeInTheDocument();
  });

  it("surfaces each attached workspace's Comes-with summary under the Workspaces row", () => {
    const ws = {
      ...localDirWorkspace(),
      requirements: { "secret:DATABASE_URL": { level: "required", provenance: "operator_set" } },
    } as Workspace;
    render(
      <StepReview
        state={{ ...initialWizardState("CC2"), workspaces: [{ workspaceId: "ws-1" }] }}
        patch={() => {}}
        workspaces={[ws]}
      />,
    );
    expect(screen.getByText("Comes with: 1 secret")).toBeInTheDocument();
  });
});

describe("StepReview — Model access (resolved from integrations)", () => {
  it("shows the resolved integration's name + type", async () => {
    listIntegrationsMock.mockResolvedValueOnce({ ai: [teamKey], scm: [], mirror: [], proxy: [] });
    render(<StepReview state={initialWizardState("CC2")} patch={() => {}} />);
    expect(await screen.findByText("Team API key")).toBeInTheDocument();
    expect(screen.getByText("anthropic · api key")).toBeInTheDocument();
  });

  it("shows the honest amber line when nothing resolves", async () => {
    listIntegrationsMock.mockResolvedValueOnce({ ai: [], scm: [], mirror: [], proxy: [] });
    render(<StepReview state={initialWizardState("CC2")} patch={() => {}} />);
    await screen.findByText("Model access");
    expect(await screen.findByText(/No integration can drive Claude Code/)).toBeInTheDocument();
  });

  // With BOTH a managed subscription and a ready Bedrock configured, dispatch
  // runs on Bedrock (runs_dispatch_llm.go: `managed := … && !t.bedrockReady`).
  // Naming the subscription — merely the row deriveAiRows pushes first — also
  // reads its proxy_injected residency, and this chip is gated on the NAMED
  // row's residency tone: the operator would be told nothing is resident while
  // the run carries AWS keys in the sandbox. Wrong direction for a product
  // whose thesis is credential containment.
  it("names Bedrock and still shows the resident-credential chip when a managed subscription is also configured", async () => {
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
    listIntegrationsMock.mockResolvedValueOnce({ ai: [managedSubscription, bedrock], scm: [], mirror: [], proxy: [] });
    getSetupStatusMock.mockResolvedValueOnce(
      baseStatus({
        integrations: [
          { id: "anthropic_subscription:managed", category: "ai_provider", type: "anthropic_subscription" },
          { id: "bedrock", category: "ai_provider", type: "bedrock" },
        ],
        bedrock: { region: "us-east-1", model: "anthropic.claude-3-sonnet", creds_present: true, ready: true },
        harness: [{ provider: "anthropic", captured: true }],
      }),
    );
    render(<StepReview state={initialWizardState("CC2")} patch={() => {}} />);
    expect(await screen.findByText("AWS Bedrock")).toBeInTheDocument();
    expect(screen.queryByText("Claude subscription (managed)")).not.toBeInTheDocument();
    expect(screen.getByText("Reduced isolation: credential resident in sandbox")).toBeInTheDocument();
  });

  it("shows the governed-command line instead of a resolution for a governed command", () => {
    render(
      <StepReview
        state={{ ...initialWizardState("CC2"), runType: "command" }}
        patch={() => {}}
      />,
    );
    expect(screen.getByText(/Governed command — no model access is wired/)).toBeInTheDocument();
  });

  // Belt-and-suspenders on top of resolveModelAccess's own global-provider
  // carve-out (step-access.test.tsx): even if nothing resolves client-side,
  // the Summary must never contradict a preflight that already says
  // llm_access is satisfied — the same rule the warning box above it follows.
  it("does not contradict a satisfied preflight llm_access item even when nothing resolves client-side", async () => {
    listIntegrationsMock.mockResolvedValueOnce({ ai: [], scm: [], mirror: [], proxy: [] });
    render(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={preflight({
          setup_items: [
            {
              // Distinct from the Summary's own "Model access" label below —
              // this is realistic checklist copy for the Bedrock carve-out
              // (preflight.go:170-179's Note), not a text-collision risk.
              id: "llm_access",
              kind: "llm_access",
              label: "Amazon Bedrock configured",
              required_by: "the agent",
              status: "satisfied",
            },
          ],
        })}
        preflightStatus="idle"
      />,
    );
    await screen.findByText("Model access");
    expect(screen.queryByText(RD.NONE_LINE)).not.toBeInTheDocument();
    expect(screen.queryByTestId("review-no-model-access")).not.toBeInTheDocument();
    expect(screen.getByText("Provisioned — see the checklist above.")).toBeInTheDocument();
  });
});
