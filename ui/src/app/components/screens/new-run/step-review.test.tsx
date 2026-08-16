/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PreflightResult, Workspace } from "../../../lib/types";
import type { IntegrationRow } from "../../../lib/api/integrations";
import { StepReview } from "./step-review";
import { initialWizardState } from "./wizard-types";
import { baseStatus } from "../../../lib/test-fixtures";
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
  baseStatus({ integrations: [{ id: "anthropic_api_key", kind: "anthropic_api_key", default_for: ["agent_runs"] }] }),
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

  it("surfaces preflight clamp warnings (a member's tightened inline_policy) and omits the block when there are none", () => {
    const { rerender } = render(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={preflight({ warnings: ["Dropped grant for attacker.example (not operator-eligible)"] })}
        preflightStatus="idle"
      />,
    );
    const block = screen.getByTestId("preflight-clamp-warnings");
    expect(block).toHaveTextContent(/Tightened by policy/i);
    expect(block).toHaveTextContent(/not operator-eligible/i);

    // No warnings (the common case, and an older server that omits the field) ⇒ no block.
    rerender(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={preflight()}
        preflightStatus="idle"
      />,
    );
    expect(screen.queryByTestId("preflight-clamp-warnings")).toBeNull();
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

  it("adds the autonomous-run wording to the no-model warning in batch mode", async () => {
    // Default state: claude-code + apikey + no stored secret => noLlmCred.
    render(
      <StepReview
        state={{ ...initialWizardState("CC2"), mode: "batch" }}
        patch={() => {}}
        preflight={preflight()}
        preflightStatus="idle"
      />,
    );
    // UI-RUN-2: the box is gated on `loaded` (the self-fetches settling) —
    // await it rather than asserting the (necessarily correct, but PREMATURE)
    // first paint.
    expect(await screen.findByTestId("review-no-model-access")).toBeInTheDocument();
    expect(screen.getByTestId("review-batch-no-model")).toHaveTextContent(
      /autonomous run can't perform its task without model access/i,
    );
  });

  it("omits the autonomous wording for an interactive run", async () => {
    render(
      <StepReview
        state={{ ...initialWizardState("CC2"), mode: "interactive" }}
        patch={() => {}}
        preflight={preflight()}
        preflightStatus="idle"
      />,
    );
    // The no-model warning still shows (no LLM cred), but without the batch clause.
    expect(await screen.findByTestId("review-no-model-access")).toBeInTheDocument();
    expect(screen.queryByTestId("review-batch-no-model")).toBeNull();
  });

  // D3/claim2: the old remedy ("Go back to Access and pick a stored key") named
  // a control step-access.tsx no longer has — model access resolves from
  // integrations, and there is no manual picker on Access to "go back" to.
  it("the no-model-access remedy names Integrations, not the deleted Access picker", async () => {
    render(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={preflight()}
        preflightStatus="idle"
      />,
    );
    const banner = await screen.findByTestId("review-no-model-access");
    expect(banner).toHaveTextContent(/Connect a provider under Integrations/);
    expect(banner).not.toHaveTextContent(/pick a stored key/i);
    expect(banner).not.toHaveTextContent(/go back to access/i);
  });
});

// UI-RUN-2: StepReview had no loaded-gate on its OWN self-fetches (unlike its
// sibling ModelAccessCard, M5) — before they settle, `resolved` is computed
// off default/empty state, so the final gate screen asserted "No model
// access" on every entry, even for a working pinned integration, before
// silently retracting it once the fetch landed.
describe("StepReview — Model access loaded-gate (UI-RUN-2)", () => {
  it("shows the resolving line and suppresses the no-model-access box before the self-fetches settle", () => {
    // Never-resolving (Once, so it doesn't leak into later tests in this
    // file): assert the very FIRST paint, before any effect's .then/.finally
    // has had a chance to run.
    listIntegrationsMock.mockReturnValueOnce(new Promise(() => {}));
    render(<StepReview state={initialWizardState("CC2")} patch={() => {}} />);
    expect(screen.getByText(RD.RESOLVING_LINE)).toBeInTheDocument();
    expect(screen.queryByTestId("review-no-model-access")).toBeNull();
    expect(screen.queryByText(RD.NONE_LINE("Claude Code"))).toBeNull();
  });

  it("treats a still-loading preflight as unresolved too, not just the two self-fetches", () => {
    render(
      <StepReview state={initialWizardState("CC2")} patch={() => {}} preflightStatus="loading" />,
    );
    expect(screen.getByText(RD.RESOLVING_LINE)).toBeInTheDocument();
    expect(screen.queryByTestId("review-no-model-access")).toBeNull();
  });

  it("resolves to the honest amber line once every input has settled with nothing to resolve", async () => {
    render(<StepReview state={initialWizardState("CC2")} patch={() => {}} preflightStatus="idle" />);
    expect(await screen.findByTestId("review-no-model-access")).toBeInTheDocument();
    expect(screen.getByText(RD.NONE_LINE("Claude Code"))).toBeInTheDocument();
    expect(screen.queryByText(RD.RESOLVING_LINE)).toBeNull();
  });
});

// N1: the manual wizard used to have no risk grade at all, so "Edit in
// wizard" silently walked the operator around the composer's HIGH-only
// acknowledgment gate. StepReview renders the SAME RiskPanel compose-review.tsx
// does (compose-review.test.tsx covers the panel's own rendering rules in
// full — badge, attribution, tone, rationale list); these tests cover only
// StepReview's OWN wiring: when it renders the panel at all, and that the
// acknowledgment callback reaches the caller (the wizard, which owns the bit
// and gates Launch on it — see wizard.test.tsx for that half).
describe("StepReview — risk panel (N1)", () => {
  it("renders the RiskPanel with its ack gate when preflight carries a HIGH item, and forwards the checkbox click", async () => {
    const onAcknowledge = vi.fn();
    render(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={preflight({
          risk_assessment: [
            {
              field: "allow_all_egress",
              value: "true",
              risk_level: "high",
              rationale: "Reaches almost any public host.",
            },
          ],
          overall_risk: "high",
        })}
        preflightStatus="idle"
        acknowledged={false}
        onAcknowledge={onAcknowledge}
      />,
    );
    expect(screen.getByText(/^Risk:$/)).toBeInTheDocument();
    expect(screen.getByTestId("high-risk-section")).toBeInTheDocument();
    expect(screen.getByText(/Reaches almost any public host/)).toBeInTheDocument();

    await userEvent.setup().click(screen.getByRole("checkbox"));
    expect(onAcknowledge).toHaveBeenCalledWith(true);
  });

  it("renders a neutral panel with no ack gate when preflight's risk_assessment has no HIGH item", () => {
    render(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={preflight({
          risk_assessment: [
            {
              field: "min_confinement_class",
              value: "CC2",
              risk_level: "medium",
              rationale: "Wall (the default tier — a gVisor sandbox).",
            },
          ],
          overall_risk: "medium",
        })}
        preflightStatus="idle"
      />,
    );
    expect(screen.getByText(/^Risk:$/)).toBeInTheDocument();
    expect(screen.queryByTestId("high-risk-section")).toBeNull();
    expect(screen.queryByRole("checkbox")).toBeNull();
  });

  it("renders no risk panel at all when preflight carries no risk_assessment (older server, or preflight unavailable)", () => {
    render(
      <StepReview
        state={initialWizardState("CC2")}
        patch={() => {}}
        preflight={preflight()}
        preflightStatus="idle"
      />,
    );
    expect(screen.queryByText(/^Risk:$/)).toBeNull();
  });
});

// N4: the same wire value used to read "Off" everywhere — reading as LESS
// restrictive than always_deny actually is on a normal allowlist. "Off" is
// only honest once allow-all makes the setting genuinely inert.
describe("StepReview — First-use approval label (N4)", () => {
  it("reads 'Always deny' for a real always_deny choice on a normal allowlist", () => {
    render(
      <StepReview state={{ ...initialWizardState("CC2"), firstUseApproval: "always_deny" }} patch={() => {}} />,
    );
    expect(screen.getByText("Always deny")).toBeInTheDocument();
  });

  it("reads 'Off (allow-all)' once egress is allow-all — buildSpec forces always_deny there, and it's genuinely inert", () => {
    render(<StepReview state={{ ...initialWizardState("CC2"), allowAllEgress: true }} patch={() => {}} />);
    expect(screen.getByText("Off (allow-all)")).toBeInTheDocument();
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
    // UX-4: the sub-line now comes from the shared composition-aware sourceSubLine
    // (a single local dir shows its path; a multi-source workspace would read
    // "2 dirs · 1 repo") — never the synthetic "Repo: local:<basename>" label.
    // The path also appears in the enforced-policy JSON below, hence getAllByText.
    expect(screen.getAllByText("/home/me/payments").length).toBeGreaterThan(0);
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

  // Item 4 (found by live driving): checking an Optional row in Basics — e.g.
  // telemetry.example.com, verified live to persist across Back/Next — never
  // moved this line. The whole premise of workspace-first run creation is
  // "start from the workspace and edit from there"; the final screen (Review)
  // must show the edit, not just repeat the static Required contract.
  it("reflects an enabled Optional row (this run's opt-in), not just the static Required contract", () => {
    const ws = {
      ...localDirWorkspace(),
      requirements: {
        "secret:DATABASE_URL": { level: "required", provenance: "operator_set" },
        "egress:telemetry.example.com": { level: "optional", provenance: "operator_set" },
      },
    } as Workspace;
    render(
      <StepReview
        state={{
          ...initialWizardState("CC2"),
          workspaces: [
            { workspaceId: "ws-1", enabledOptional: ["egress:telemetry.example.com"] },
          ],
        }}
        patch={() => {}}
        workspaces={[ws]}
      />,
    );
    expect(screen.getByText("Comes with: 1 secret · 1 opted in")).toBeInTheDocument();
  });
});

// D6/claim3: the Egress row used to print a bare count — "3 allowed" — so an
// operator who trimmed the allow-list to one host had to open the raw JSON to
// learn which two hosts a grant silently added. Names them now, truncated so
// a long list doesn't blow out the summary grid.
describe("StepReview — Egress row shows host NAMES, not a bare count (D6/claim3)", () => {
  it("comma-lists the allowed hosts", () => {
    render(
      <StepReview
        state={{ ...initialWizardState("CC2"), allowedDomains: ["api.anthropic.com", "github.com"] }}
        patch={() => {}}
      />,
    );
    expect(screen.getByText("api.anthropic.com, github.com")).toBeInTheDocument();
  });

  it("truncates beyond ~4 hosts with a '+N more' suffix", () => {
    render(
      <StepReview
        state={{
          ...initialWizardState("CC2"),
          allowedDomains: ["a.com", "b.com", "c.com", "d.com", "e.com", "f.com"],
        }}
        patch={() => {}}
      />,
    );
    expect(screen.getByText("a.com, b.com, c.com, d.com, +2 more")).toBeInTheDocument();
  });

  it("still appends the denied count alongside the truncated host list", () => {
    render(
      <StepReview
        state={{
          ...initialWizardState("CC2"),
          allowedDomains: ["api.anthropic.com"],
          deniedDomains: ["evil.example.com"],
        }}
        patch={() => {}}
      />,
    );
    expect(screen.getByText("api.anthropic.com, 1 denied")).toBeInTheDocument();
  });

  // The sharpest sub-case from claim 3: a repo workspace silently unions
  // github.com/*.githubusercontent.com into allowed_domains — now visible by
  // NAME on the one screen whose job is showing what's about to launch.
  // Implied hosts render FIRST: buildSpec appends them last, so first-4
  // truncation on a long allowlist would hide exactly these (review F2).
  it("includes grant-implied hosts by name (a repo workspace's github.com)", () => {
    const repoWs = {
      id: "ws-1",
      name: "app",
      kind: "repo",
      source: "acme/app",
      status: "scanned",
      created_at: "",
      updated_at: "",
    } as Workspace;
    render(
      <StepReview
        state={{
          ...initialWizardState("CC2"),
          allowedDomains: ["api.anthropic.com"],
          workspaces: [{ workspaceId: "ws-1" }],
        }}
        patch={() => {}}
        workspaces={[repoWs]}
      />,
    );
    expect(screen.getByText("github.com, *.githubusercontent.com, api.anthropic.com")).toBeInTheDocument();
  });

  // Implied-first is the load-bearing half of the fix: with ≥4 operator
  // presets, first-4 truncation used to drop the grant-added tail entirely,
  // degrading the row back into the count-only defect it replaced.
  it("keeps implied hosts visible ahead of a long operator allowlist", () => {
    const repoWs = {
      id: "ws-1",
      name: "app",
      kind: "repo",
      source: "acme/app",
      status: "scanned",
      created_at: "",
      updated_at: "",
    } as Workspace;
    render(
      <StepReview
        state={{
          ...initialWizardState("CC2"),
          allowedDomains: ["a.com", "b.com", "c.com", "d.com", "e.com"],
          workspaces: [{ workspaceId: "ws-1" }],
        }}
        patch={() => {}}
        workspaces={[repoWs]}
      />,
    );
    expect(
      screen.getByText("github.com, *.githubusercontent.com, a.com, b.com, +3 more"),
    ).toBeInTheDocument();
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
          { id: "anthropic_subscription:managed", kind: "anthropic_subscription" },
          { id: "bedrock", kind: "bedrock" },
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
    // Waits out the self-fetches (UI-RUN-2's loaded gate) — the terminal
    // "Provisioned" text only ever renders once `loaded` is true.
    expect(await screen.findByText("Provisioned — see the checklist above.")).toBeInTheDocument();
    expect(screen.queryByText(RD.NONE_LINE("Claude Code"))).not.toBeInTheDocument();
    expect(screen.queryByTestId("review-no-model-access")).not.toBeInTheDocument();
  });
});
