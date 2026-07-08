/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ComposeReview } from "./compose-review";
import { RISK_ATTRIBUTION } from "../../wardyn/copy";
import type { ComposeResponse, SetupItem } from "../../../lib/types";

// The Proposed Setup review screen must (1) show the deterministic overall risk
// grade with its attribution (Wardyn's rules, not the model), and (2) GATE launch
// behind an explicit acknowledgment whenever there are HIGH-risk items — Launch run
// stays disabled until the operator checks the box. A proposal with no high items
// launches without a gate.

function baseResult(overrides: Partial<ComposeResponse> = {}): ComposeResponse {
  return {
    kind: "proposal",
    proposed: {
      run: {
        agent: "claude-code",
        repo: "acme/payments",
        task: "fix the flaky test",
        confinement_class: "CC2",
        interactive: false,
      },
      inline_policy: {
        allowed_domains: ["api.anthropic.com"],
        first_use_approval: "deny_with_review",
        min_confinement_class: "CC2",
        auto_stop_after_sec: 7200,
      },
    },
    risk_assessment: [
      {
        field: "min_confinement_class",
        value: "CC2",
        risk_level: "medium",
        rationale: "gVisor sandbox.",
      },
      {
        field: "first_use_approval",
        value: "true",
        risk_level: "low",
        rationale: "Operator confirms first egress to each new host.",
      },
    ],
    overall_risk: "medium",
    summary: "A confined batch run.",
    ...overrides,
  };
}

function highRiskResult(): ComposeResponse {
  return baseResult({
    proposed: {
      run: {
        agent: "claude-code",
        repo: "acme/payments",
        task: "fix the flaky test",
        confinement_class: "CC1",
        interactive: false,
      },
      inline_policy: {
        allowed_domains: [],
        first_use_approval: "always_deny",
        min_confinement_class: "CC1",
        allow_all_egress: true,
      },
    },
    risk_assessment: [
      {
        field: "min_confinement_class",
        value: "CC1",
        risk_level: "high",
        rationale: "Permissive runc sandbox — weakest isolation.",
        invariant_ref: "3",
      },
      {
        field: "allow_all_egress",
        value: "true",
        risk_level: "high",
        rationale: "Deny-list only egress; any non-denied host is reachable.",
      },
      {
        field: "first_use_approval",
        value: "false",
        risk_level: "medium",
        rationale: "No human gate on first egress.",
      },
    ],
    overall_risk: "high",
  });
}

function renderReview(
  result: ComposeResponse,
  opts: {
    acknowledged?: boolean;
    interactive?: boolean;
    setupItems?: SetupItem[];
    onAddSecret?: (name: string) => void;
    onFixWorkspace?: (workspaceId: string) => void;
  } = {},
) {
  const onAcknowledge = vi.fn();
  const onApproveLaunch = vi.fn();
  const onEditInWizard = vi.fn();
  const onCancel = vi.fn();
  const onInteractiveChange = vi.fn();
  const onAddSecret = opts.onAddSecret ?? vi.fn();
  const onFixWorkspace = opts.onFixWorkspace ?? vi.fn();
  const utils = render(
    <ComposeReview
      result={result}
      setupItems={opts.setupItems}
      interactive={opts.interactive ?? false}
      acknowledged={opts.acknowledged ?? false}
      launching={false}
      onInteractiveChange={onInteractiveChange}
      onAcknowledge={onAcknowledge}
      onApproveLaunch={onApproveLaunch}
      onAddSecret={onAddSecret}
      onFixWorkspace={onFixWorkspace}
      onEditInWizard={onEditInWizard}
      onCancel={onCancel}
    />,
  );
  return {
    onAcknowledge,
    onApproveLaunch,
    onEditInWizard,
    onCancel,
    onInteractiveChange,
    onAddSecret,
    onFixWorkspace,
    ...utils,
  };
}

describe("ComposeReview — risk grade", () => {
  it("shows the overall risk badge, its deterministic attribution, and the title/summary", () => {
    const { container } = renderReview(baseResult());
    expect(screen.getByText(/^Risk:$/)).toBeInTheDocument();
    // Attribution makes clear Wardyn's rules graded this, not the model.
    expect(screen.getByText(new RegExp(RISK_ATTRIBUTION, "i"))).toBeInTheDocument();
    // The overall grade drives the RiskBadge hook.
    expect(container.querySelector('[data-risk="medium"]')).not.toBeNull();
    // Title (run.task) and inert model summary lead the screen.
    expect(screen.getByRole("heading", { name: /fix the flaky test/i })).toBeInTheDocument();
    expect(screen.getByText("A confined batch run.")).toBeInTheDocument();
  });

  it("renders the identity facts, incl. the barrier by label with NO CCx leak", () => {
    const { container } = renderReview(baseResult());
    // Barrier shows "Wall" (label), never the wire class on its face.
    expect(screen.getByText("Wall")).toBeInTheDocument();
    expect(screen.getByText("acme/payments")).toBeInTheDocument();
    expect(screen.getByText(/auto-stops after 2h/i)).toBeInTheDocument();
    // The wire code is allowed ONLY inside the "exact policy" operator escape hatch.
    const details = container.querySelector("details");
    expect(details?.textContent).toMatch(/CC2/);
    // Everything OUTSIDE it — the human summary, chips, risk panel — must not leak
    // the wire class or invariant refs (D4).
    details?.remove();
    expect(container.textContent).not.toMatch(/CC2/);
    expect(container.textContent).not.toMatch(/invariant/i);
  });

  it("renders warnings when present", () => {
    renderReview(baseResult({ warnings: ["clamped allowed_domains to ceiling"] }));
    expect(screen.getByText(/clamped allowed_domains to ceiling/i)).toBeInTheDocument();
  });

  it("never renders the word 'unrestricted', even for allow_all at HIGH risk", () => {
    const { container } = renderReview(highRiskResult());
    expect(container.textContent).not.toMatch(/unrestricted/i);
  });
});

describe("ComposeReview — mode selector (D3: Interactive / Autonomous)", () => {
  it("renders an Interactive/Autonomous toggle reflecting the current mode", () => {
    renderReview(baseResult(), { interactive: false });
    const group = screen.getByRole("radiogroup", { name: /run mode/i });
    expect(within(group).getByRole("radio", { name: "Interactive" })).toHaveAttribute(
      "aria-checked",
      "false",
    );
    expect(within(group).getByRole("radio", { name: "Autonomous" })).toHaveAttribute(
      "aria-checked",
      "true",
    );
  });

  it("fires onInteractiveChange when the operator overrides the proposed mode", async () => {
    const { onInteractiveChange } = renderReview(baseResult(), { interactive: false });
    await userEvent.setup().click(screen.getByRole("radio", { name: "Interactive" }));
    expect(onInteractiveChange).toHaveBeenCalledWith(true);
  });
});

describe("ComposeReview — high-risk acknowledgment gate (D8)", () => {
  it("disables Launch run until the high-risk box is acknowledged", async () => {
    const { rerender, onAcknowledge, onApproveLaunch } = renderReview(highRiskResult());
    const user = userEvent.setup();

    expect(screen.getByText(/high-risk configuration/i)).toBeInTheDocument();

    const launch = screen.getByRole("button", { name: /approve & launch/i });
    expect(launch).toBeDisabled();

    const ack = screen.getByRole("checkbox");
    await user.click(ack);
    expect(onAcknowledge).toHaveBeenCalledWith(true);

    rerender(
      <ComposeReview
        result={highRiskResult()}
        interactive={false}
        acknowledged={true}
        launching={false}
        onInteractiveChange={() => {}}
        onAcknowledge={onAcknowledge}
        onApproveLaunch={onApproveLaunch}
        onEditInWizard={() => {}}
        onCancel={() => {}}
      />,
    );
    const launchEnabled = screen.getByRole("button", { name: /approve & launch/i });
    expect(launchEnabled).toBeEnabled();
    await user.click(launchEnabled);
    expect(onApproveLaunch).toHaveBeenCalledTimes(1);
  });

  it("lists each high item by its plain rationale (no wire field / invariant leak)", () => {
    renderReview(highRiskResult(), { acknowledged: false });
    const section = screen.getByTestId("high-risk-section");
    expect(within(section).getByText(/weakest isolation/i)).toBeInTheDocument();
    expect(within(section).getByText(/any non-denied host is reachable/i)).toBeInTheDocument();
    // The medium item is NOT escalated into the high-risk gate.
    expect(within(section).queryByText(/no human gate on first egress/i)).toBeNull();
    // No raw wire field names in the gate.
    expect(within(section).queryByText(/min_confinement_class/)).toBeNull();
  });

  it("enables launch immediately when there are no high-risk items (no gate)", () => {
    renderReview(baseResult());
    expect(screen.queryByTestId("high-risk-section")).toBeNull();
    expect(screen.getByRole("button", { name: /approve & launch/i })).toBeEnabled();
  });

  it("Adjust in full wizard and Cancel fire their callbacks", async () => {
    const { onEditInWizard, onCancel } = renderReview(baseResult());
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: /edit in wizard/i }));
    expect(onEditInWizard).toHaveBeenCalledTimes(1);
    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });
});

// Doctor-style setup checklist (decision 2/3/4): declared-present, deterministic,
// non-blocking. Each row is label+detail | StatusChip | fix action.
describe("ComposeReview — setup checklist", () => {
  const secretItem: SetupItem = {
    id: "secret:dazz-pg-credentials",
    kind: "secret",
    label: "dazz-pg-credentials",
    required_by: "the api_key grant",
    status: "missing",
    fix: { action: "add_secret", secret_name: "dazz-pg-credentials" },
  };
  const workspaceItem: SetupItem = {
    id: "workspace:ws-1",
    kind: "workspace",
    label: "payments repo",
    required_by: "the run's workspace",
    status: "missing",
    detail: "Still scanning — not ready yet.",
    fix: { action: "scan_workspace", workspace_id: "ws-1" },
  };
  const satisfiedItem: SetupItem = {
    id: "workspace:ws-2",
    kind: "workspace",
    label: "widgets repo",
    required_by: "the run's workspace",
    status: "satisfied",
  };
  const unverifiedItem: SetupItem = {
    id: "repo_credential:github_token",
    kind: "repo_credential",
    label: "GitHub access",
    required_by: "the github_token grant",
    status: "unverified",
    detail: "Minted at launch time — can't be checked in advance.",
  };

  it("renders no checklist section when setupItems is absent/empty", () => {
    renderReview(baseResult());
    expect(screen.queryByText(/setup checklist/i)).toBeNull();
  });

  it("renders each item's label, detail, and status copy", () => {
    renderReview(baseResult(), { setupItems: [secretItem, workspaceItem, satisfiedItem, unverifiedItem] });
    expect(screen.getByText("Setup checklist")).toBeInTheDocument();

    const secretRow = screen.getByTestId(`setup-item-${secretItem.id}`);
    expect(within(secretRow).getByText("dazz-pg-credentials")).toBeInTheDocument();
    expect(within(secretRow).getByText(/required by the api_key grant/i)).toBeInTheDocument();
    expect(within(secretRow).getByText("Needs setup")).toBeInTheDocument();

    // "satisfied" renders "Configured" — NEVER "Ready"/"Verified" (decision 3: v1
    // is declared-present, not live-verified).
    const satisfiedRow = screen.getByTestId(`setup-item-${satisfiedItem.id}`);
    expect(within(satisfiedRow).getByText("Configured")).toBeInTheDocument();
    expect(within(satisfiedRow).queryByText(/^ready$/i)).toBeNull();
    expect(within(satisfiedRow).queryByText(/verified/i)).toBeNull();

    const unverifiedRow = screen.getByTestId(`setup-item-${unverifiedItem.id}`);
    expect(within(unverifiedRow).getByText("Unverified")).toBeInTheDocument();
    expect(within(unverifiedRow).getByText(/can't be checked in advance/i)).toBeInTheDocument();
  });

  it("gives a missing secret/llm_access item destructive-style; a missing workspace item stays plain (decision 4)", () => {
    renderReview(baseResult(), { setupItems: [secretItem, workspaceItem] });
    expect(screen.getByTestId(`setup-item-${secretItem.id}`).className).toMatch(/border-danger/);
    expect(screen.getByTestId(`setup-item-${workspaceItem.id}`).className).not.toMatch(/border-danger/);
  });

  it("a missing secret item's fix calls onAddSecret with Fix.SecretName", async () => {
    const { onAddSecret } = renderReview(baseResult(), { setupItems: [secretItem] });
    const row = screen.getByTestId(`setup-item-${secretItem.id}`);
    await userEvent.setup().click(within(row).getByRole("button", { name: /add secret/i }));
    expect(onAddSecret).toHaveBeenCalledWith("dazz-pg-credentials");
  });

  it("a missing workspace item's fix calls onFixWorkspace with Fix.WorkspaceID", async () => {
    const { onFixWorkspace } = renderReview(baseResult(), { setupItems: [workspaceItem] });
    const row = screen.getByTestId(`setup-item-${workspaceItem.id}`);
    await userEvent.setup().click(within(row).getByRole("button", { name: /scan workspace/i }));
    expect(onFixWorkspace).toHaveBeenCalledWith("ws-1");
  });

  it("renders the backend/config_pair kinds and keeps a missing one non-destructive (amber, not danger)", () => {
    const backendItem: SetupItem = {
      id: "backend:CC2",
      kind: "backend",
      label: "Sandbox barrier: Wall",
      required_by: "the proposal's confinement class",
      status: "missing",
      detail: "no Wall (gVisor) runtime registered on this host yet.",
    };
    const configPairItem: SetupItem = {
      id: "config_pair:use_subscription:claude_cred_mount",
      kind: "config_pair",
      label: "Paired setting: subscription mode + credential mount",
      required_by: "the requested Claude subscription transport",
      status: "missing",
      detail: "subscription mode was requested but not applied.",
    };
    renderReview(baseResult(), { setupItems: [backendItem, configPairItem] });

    const backendRow = screen.getByTestId(`setup-item-${backendItem.id}`);
    const configPairRow = screen.getByTestId(`setup-item-${configPairItem.id}`);
    expect(within(backendRow).getByText("Needs setup")).toBeInTheDocument();
    expect(within(configPairRow).getByText("Needs setup")).toBeInTheDocument();
    // Neither gets the destructive treatment a missing llm_access/secret gets —
    // they're host/config state, not a credential absence (decision 4).
    expect(backendRow.className).not.toMatch(/border-danger/);
    expect(configPairRow.className).not.toMatch(/border-danger/);
  });

  it("renders a workspace_secret 'missing' item NON-destructive with an Add-secret fix wired to secret_name", async () => {
    // A secret a mounted workspace's OWN files declare — the run still launches, so
    // the row must fall through the destructive guard (kind-gated to llm_access|secret)
    // and stay amber, while still offering the generic add_secret fix.
    const wsSecretItem: SetupItem = {
      id: "workspace_secret:stripe-key",
      kind: "workspace_secret",
      label: "stripe-key",
      required_by: "the payments workspace's declared secrets",
      status: "missing",
      fix: { action: "add_secret", secret_name: "stripe-key" },
    };
    const { onAddSecret } = renderReview(baseResult(), { setupItems: [wsSecretItem] });
    const row = screen.getByTestId(`setup-item-${wsSecretItem.id}`);
    expect(row.className).not.toMatch(/border-danger/);
    expect(within(row).getByText("Needs setup")).toBeInTheDocument();
    await userEvent.setup().click(within(row).getByRole("button", { name: /add secret/i }));
    expect(onAddSecret).toHaveBeenCalledWith("stripe-key");
  });

  it("shows the residency sub-line for each residency value, and none when absent", () => {
    const proxyItem: SetupItem = {
      id: "secret:anthropic-api-key",
      kind: "secret",
      label: "Secret: anthropic-api-key",
      required_by: "an api_key grant",
      status: "satisfied",
      residency: "proxy_injected",
    };
    const mountItem: SetupItem = {
      id: "llm_access:claude-code",
      kind: "llm_access",
      label: "Model access for claude-code",
      required_by: "the agent's own model calls",
      status: "satisfied",
      residency: "resident_mount",
    };
    const brokeredItem: SetupItem = {
      id: "repo_credential:github_token",
      kind: "repo_credential",
      label: "GitHub repository access",
      required_by: "cloning/pushing the workspace's GitHub remote",
      status: "unverified",
      residency: "brokered_mint",
    };
    const noneItem: SetupItem = {
      id: "workspace:ws-2",
      kind: "workspace",
      label: "widgets repo",
      required_by: "the run's workspace",
      status: "satisfied",
    };
    renderReview(baseResult(), { setupItems: [proxyItem, mountItem, brokeredItem, noneItem] });

    expect(
      within(screen.getByTestId(`setup-item-${proxyItem.id}`)).getByText(
        /held by the proxy — never inside the sandbox/i,
      ),
    ).toBeInTheDocument();
    expect(
      within(screen.getByTestId(`setup-item-${mountItem.id}`)).getByText(/mounted into the sandbox/i),
    ).toBeInTheDocument();
    expect(
      within(screen.getByTestId(`setup-item-${brokeredItem.id}`)).getByText(
        /brokered at launch by the control plane/i,
      ),
    ).toBeInTheDocument();
    // No residency => no sub-line rendered at all for that row.
    expect(screen.getByTestId(`setup-item-${noneItem.id}`).textContent).not.toMatch(
      /held by the proxy|mounted into the sandbox|brokered at launch/i,
    );
  });

  it("wires the no-model-access banner's action button to the llm_access item's fix", async () => {
    const llmAccessItem: SetupItem = {
      id: "llm_access:anthropic-api-key",
      kind: "llm_access",
      label: "Model access",
      required_by: "claude-code",
      status: "missing",
      fix: { action: "add_secret", secret_name: "anthropic-api-key" },
    };
    const { onAddSecret } = renderReview(
      baseResult({ llm_access: { provisioned: false, note: "No Anthropic key or Claude login found." } }),
      { setupItems: [llmAccessItem] },
    );
    const banner = screen.getByTestId("no-model-access");
    await userEvent.setup().click(within(banner).getByRole("button", { name: /add the .*anthropic-api-key.* secret/i }));
    expect(onAddSecret).toHaveBeenCalledWith("anthropic-api-key");
  });
});
