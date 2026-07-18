/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { AgentRun, Workspace } from "../../../lib/types";

// H1/H3 regression: a launch failure naming a not-yet-stored secret (the
// stored/default policy path now 422s on this too — H1) must offer the same
// one-click "add it and retry" fix the composer review panel does (926da19),
// ported onto the manual wizard's plain-text error banner.

const toastError = vi.fn();
const toastSuccess = vi.fn();
const toastWarning = vi.fn();
vi.mock("sonner", () => ({
  toast: {
    error: (...a: unknown[]) => toastError(...a),
    success: (...a: unknown[]) => toastSuccess(...a),
    warning: (...a: unknown[]) => toastWarning(...a),
  },
}));

const healthMock = vi.fn();
const listSecretsMock = vi.fn();
const listWorkspacesMock = vi.fn();
const profileRunMock = vi.fn();
const createRunMock = vi.fn();
const createPolicyMock = vi.fn();
const setSecretMock = vi.fn();
const preflightRunMock = vi.fn();

vi.mock("../../../lib/api/health", () => ({
  health: { health: (...a: unknown[]) => healthMock(...a) },
}));
vi.mock("../../../lib/api/secrets", () => ({
  secrets: {
    listSecrets: (...a: unknown[]) => listSecretsMock(...a),
    setSecret: (...a: unknown[]) => setSecretMock(...a),
  },
}));
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a),
    scanWorkspace: vi.fn(),
  },
}));
vi.mock("../../../lib/api/policies", () => ({
  policies: {
    listPolicies: () => Promise.resolve([]),
    createPolicy: (...a: unknown[]) => createPolicyMock(...a),
  },
}));
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    profileRun: (...a: unknown[]) => profileRunMock(...a),
    createRun: (...a: unknown[]) => createRunMock(...a),
    preflightRun: (...a: unknown[]) => preflightRunMock(...a),
  },
}));

import { PermissionWizard } from "./wizard";
import { initialWizardState } from "./wizard-types";

const workspace: Workspace = {
  id: "ws-1",
  name: "acme-repo",
  kind: "repo",
  source: "acme/widgets",
  status: "ready",
  created_at: "now",
  updated_at: "now",
};

const createdRun: AgentRun = {
  id: "run-1",
  created_at: "now",
  updated_at: "now",
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "",
  confinement_class: "CC2",
  state: "PENDING",
  spiffe_id: "spiffe://x",
  runner_target: "docker",
};

describe("PermissionWizard — launch-error missing-secret fix (H1/H3)", () => {
  beforeEach(() => {
    toastError.mockReset();
    toastSuccess.mockReset();
    toastWarning.mockReset();
    healthMock.mockReset();
    listSecretsMock.mockReset();
    listWorkspacesMock.mockReset();
    createRunMock.mockReset();
    createPolicyMock.mockReset();
    profileRunMock.mockReset();
    setSecretMock.mockReset();
    preflightRunMock.mockReset();

    healthMock.mockResolvedValue({ confinement_classes: ["CC1", "CC2", "CC3"] });
    listSecretsMock.mockResolvedValue([]);
    listWorkspacesMock.mockResolvedValue([workspace]);
    setSecretMock.mockResolvedValue(undefined);
    // Preflight fires when Review is entered — default to a benign checklist so
    // the many go-to-Review tests don't each have to stub it.
    preflightRunMock.mockResolvedValue({
      setup_items: [
        {
          id: "backend:CC2",
          kind: "backend",
          label: "Sandbox barrier: Wall",
          required_by: "the proposal's confinement class",
          status: "satisfied",
        },
      ],
      enforced_confinement_class: "CC2",
    });
  });

  // Prefilled state that clears every step's validateStep so we can click
  // straight through to Review/Launch without touching every field by hand.
  function readyState() {
    return {
      ...initialWizardState("CC2"),
      workspaces: [{ workspaceId: workspace.id }],
      llmSecretName: "anthropic-api-key",
    };
  }

  async function goToLastStep(user: ReturnType<typeof userEvent.setup>) {
    for (let i = 0; i < 4; i++) {
      await user.click(await screen.findByRole("button", { name: /^next$/i }));
    }
  }

  // Confinement is WIZARD_STEPS[3] — three "Next" clicks from Basics.
  async function goToConfinementStep(user: ReturnType<typeof userEvent.setup>) {
    for (let i = 0; i < 3; i++) {
      await user.click(await screen.findByRole("button", { name: /^next$/i }));
    }
  }

  it("offers a one-click add-secret retry when create-run 422s on a missing secret, and relaunches on save", async () => {
    createRunMock
      .mockRejectedValueOnce(
        new Error(
          'invalid policy: api_key grant references unknown secret "anthropic-api-key" (set it first via the secrets API)',
        ),
      )
      .mockResolvedValueOnce(createdRun);
    const onCreated = vi.fn();
    const onOpenChange = vi.fn();
    render(
      <PermissionWizard
        open
        onOpenChange={onOpenChange}
        onCreated={onCreated}
        initialState={readyState()}
      />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await goToLastStep(user);
    await user.click(screen.getByRole("button", { name: /launch run/i }));

    // The error is surfaced inline (never a corner toast) with the fix button.
    const banner = await screen.findByTestId("wizard-launch-error");
    expect(banner).toHaveTextContent(/unknown secret/i);
    const fixBtn = await screen.findByRole("button", {
      name: /add the .*anthropic-api-key.* secret/i,
    });

    await user.click(fixBtn);
    expect(await screen.findByText(/^add secret$/i)).toBeInTheDocument();

    // Fill the value and save — this should retry the SAME launch, not just
    // close the dialog.
    await user.type(screen.getByLabelText(/value/i), "sk-ant-newvalue");
    await user.click(screen.getByRole("button", { name: /^save secret$/i }));

    await waitFor(() => expect(setSecretMock).toHaveBeenCalledWith("anthropic-api-key", "sk-ant-newvalue"));
    await waitFor(() => expect(createRunMock).toHaveBeenCalledTimes(2));
    expect(onCreated).toHaveBeenCalledWith(createdRun);
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("loads the selected workspace's recorded egress (approved_egress) into the run", async () => {
    // The recording promoted these into the workspace; a new run should inherit them.
    const wsWithEgress: Workspace = { ...workspace, approved_egress: ["registry.npmjs.org", "api.anthropic.com"] };
    listWorkspacesMock.mockReset();
    listWorkspacesMock.mockResolvedValue([wsWithEgress]);
    createRunMock.mockResolvedValue(createdRun);
    render(
      <PermissionWizard open onOpenChange={() => {}} onCreated={() => {}} initialState={readyState()} />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await goToLastStep(user);
    await user.click(screen.getByRole("button", { name: /launch run/i }));

    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    const sent = createRunMock.mock.calls[0][0] as { inline_policy: { allowed_domains?: string[] } };
    expect(sent.inline_policy.allowed_domains).toEqual(
      expect.arrayContaining(["registry.npmjs.org", "api.anthropic.com"]),
    );
  });

  it("fast-tracks: picking a RECORDED profile on Basics jumps straight to Review", async () => {
    // The profile is the workspace's own recording (record_results) — tied to the
    // workspace by construction, not by name. Selecting it synthesizes the spec.
    const wsWithRec: Workspace = {
      ...workspace,
      record_results: {
        "build-test": { run_id: "rec-1", label: "build & test", mode: "interactive", status: "recorded" },
      },
    };
    listWorkspacesMock.mockReset();
    listWorkspacesMock.mockResolvedValue([wsWithRec]);
    profileRunMock.mockResolvedValue({
      proposed: { inline_policy: { min_confinement_class: "CC2", allowed_domains: ["registry.npmjs.org"] } },
    });
    render(
      <PermissionWizard open onOpenChange={() => {}} onCreated={() => {}} initialState={readyState()} />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    // The recording is offered as a profile; select it → its spec is synthesized.
    const profile = await screen.findByTestId("basics-profile-build-test");
    await user.click(within(profile).getByRole("radio"));
    await waitFor(() => expect(profileRunMock).toHaveBeenCalledWith("rec-1"));

    // Next becomes "Review now"; clicking it lands on the last step (Launch run).
    await user.click(await screen.findByRole("button", { name: /review now/i }));
    expect(await screen.findByRole("button", { name: /launch run/i })).toBeInTheDocument();
  });

  it("saves the named profile AFTER createRun succeeds, not before", async () => {
    createRunMock.mockResolvedValue(createdRun);
    createPolicyMock.mockResolvedValue({ id: "pol-1" });
    render(
      <PermissionWizard
        open
        onOpenChange={() => {}}
        onCreated={() => {}}
        initialState={{ ...readyState(), saveAsProfile: true, profileName: "my-profile" }}
      />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await goToLastStep(user);
    await user.click(screen.getByRole("button", { name: /launch run/i }));

    await waitFor(() => expect(createPolicyMock).toHaveBeenCalledWith("my-profile", expect.anything()));
    // createRun must have resolved before createPolicy was ever invoked — the
    // opposite order left a retry (after a failed createRun) re-hitting the
    // policies-name UNIQUE constraint with an already-saved profile.
    expect(createRunMock.mock.invocationCallOrder[0]).toBeLessThan(
      createPolicyMock.mock.invocationCallOrder[0],
    );
  });

  it("a failed createRun never creates the named policy, so a retry can't collide on the name", async () => {
    createRunMock.mockRejectedValueOnce(new Error("some launch failure")).mockResolvedValueOnce(createdRun);
    render(
      <PermissionWizard
        open
        onOpenChange={() => {}}
        onCreated={() => {}}
        initialState={{ ...readyState(), saveAsProfile: true, profileName: "my-profile" }}
      />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await goToLastStep(user);
    await user.click(screen.getByRole("button", { name: /launch run/i }));
    await screen.findByTestId("wizard-launch-error");
    expect(createPolicyMock).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /launch run/i }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(createPolicyMock).toHaveBeenCalledTimes(1));
  });

  it("does not show the fix button for a launch error that doesn't name a missing secret", async () => {
    createRunMock.mockRejectedValueOnce(new Error("confinement_class CC1 is weaker than the policy minimum CC2"));
    render(
      <PermissionWizard
        open
        onOpenChange={() => {}}
        onCreated={() => {}}
        initialState={readyState()}
      />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await goToLastStep(user);
    await user.click(screen.getByRole("button", { name: /launch run/i }));

    await screen.findByTestId("wizard-launch-error");
    expect(screen.queryByRole("button", { name: /add the .* secret/i })).toBeNull();
  });

  it("fires preflight on entering Review and renders its setup checklist", async () => {
    createRunMock.mockResolvedValue(createdRun);
    render(
      <PermissionWizard open onOpenChange={() => {}} onCreated={() => {}} initialState={readyState()} />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await goToLastStep(user);

    // Preflight was fired with the SAME inline_policy the launch would send.
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalled());
    const sent = preflightRunMock.mock.calls[0][0] as { inline_policy?: unknown };
    expect(sent.inline_policy).toBeTruthy();
    // Its checklist rows render on Review.
    expect(await screen.findByTestId("preflight-checklist")).toBeInTheDocument();
    expect(await screen.findByTestId("setup-item-backend:CC2")).toBeInTheDocument();
  });

  it("an empty first health probe retries instead of rendering a definitive unavailable barrier", async () => {
    // api.health() never rejects for real (see api.ts) — a failed/blip probe
    // resolves {} (no confinement_classes), exactly like this first call.
    healthMock
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({ confinement_classes: ["CC1", "CC2", "CC3"] });
    render(
      <PermissionWizard open onOpenChange={() => {}} onCreated={() => {}} initialState={readyState()} />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await goToConfinementStep(user);

    // Once the retry resolves with the real classes, CC2/CC3 must show enabled
    // — never stuck on the false-negative "No Wall (gVisor) runtime" verdict
    // the empty first probe would have produced pre-fix.
    await waitFor(() => expect(healthMock).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(screen.queryByText(/no wall \(gvisor\) runtime/i)).not.toBeInTheDocument(),
    );
  });

  it("falls back to the CC1-only floor only once the probe stays empty after a retry", async () => {
    healthMock.mockResolvedValue({}); // every call comes back empty
    render(
      <PermissionWizard open onOpenChange={() => {}} onCreated={() => {}} initialState={readyState()} />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await goToConfinementStep(user);

    // Retries exactly once, then honestly settles into "unavailable" — it must
    // not spin on "Checking…" forever nor commit before the retry runs.
    await waitFor(() => expect(healthMock).toHaveBeenCalledTimes(2));
    expect(await screen.findByText(/no wall \(gvisor\) runtime/i)).toBeInTheDocument();
  });
});
