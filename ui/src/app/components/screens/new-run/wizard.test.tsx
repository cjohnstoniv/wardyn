/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within, fireEvent } from "@testing-library/react";
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
const listPoliciesMock = vi.fn();
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
    listPolicies: (...a: unknown[]) => listPoliciesMock(...a),
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
  status: "scanned",
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

// 15s suite default, not vitest's 5s: these tests mount the full wizard and
// walk it with real user-event clicks through lazily-resolved steps — ~1s in
// isolation, but they flake at the 5s ceiling when the whole suite runs
// parallel on a loaded box (same rationale as setup-screen/audit's timeouts).
describe("PermissionWizard — launch-error missing-secret fix (H1/H3)", { timeout: 15_000 }, () => {
  beforeEach(() => {
    toastError.mockReset();
    toastSuccess.mockReset();
    toastWarning.mockReset();
    healthMock.mockReset();
    listSecretsMock.mockReset();
    listWorkspacesMock.mockReset();
    createRunMock.mockReset();
    createPolicyMock.mockReset();
    listPoliciesMock.mockReset();
    listPoliciesMock.mockResolvedValue([]);
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

  // UI-RUN-1 (CRITICAL): the wizard's shared add-secret handler used to write
  // EVERY manually-saved secret name into llmSecretName, regardless of which
  // "Add secret" button opened the dialog — so the Git-PAT card's OWN button
  // (the LLM secret picker it originally served was deleted) silently
  // composed an api_key grant naming the PAT for api.anthropic.com, alongside
  // the intended git_pat grant. A stored Azure DevOps PAT must never be sent
  // to Anthropic as a model key.
  it("a Git PAT saved via the card's own Add-secret button never composes an api_key grant to api.anthropic.com (UI-RUN-1)", async () => {
    render(
      <PermissionWizard
        open
        onOpenChange={() => {}}
        onCreated={() => {}}
        initialState={{ ...initialWizardState("CC2"), workspaces: [{ workspaceId: workspace.id }] }}
      />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    // Basics -> Access
    await user.click(await screen.findByRole("button", { name: /^next$/i }));

    // Enable Git PAT and set its host.
    await user.click(await screen.findByRole("switch", { name: /git pat/i }));
    await user.type(screen.getByLabelText("Host"), "dev.azure.com");

    // The card's OWN "Add secret" — the only such button on this step now
    // that the LLM secret picker is gone. Named lookup: the wizard's OWN
    // dialog ("New run") is ALSO role="dialog" and stays open underneath.
    await user.click(screen.getByRole("button", { name: /add secret/i }));
    const dialog = await screen.findByRole("dialog", { name: /^add secret$/i });
    // Secret names are lowercase-only (SECRET_NAME_RE) — "ado-pat", not the
    // finding's illustrative "ADO_PAT".
    await user.type(within(dialog).getByLabelText("Name"), "ado-pat");
    await user.type(within(dialog).getByLabelText(/value/i), "ado-pat-secret-value");
    await user.click(within(dialog).getByRole("button", { name: /^save secret$/i }));

    // Saving applies the name to the Git PAT's OWN field (gitPatSecretName) —
    // no separate manual combobox pick needed, and Access is already valid.
    await waitFor(() => expect(screen.getByText("ado-pat")).toBeInTheDocument());
    await waitFor(() => expect(screen.getByRole("button", { name: /^next$/i })).toBeEnabled());

    // Access -> Egress -> Confinement -> Review
    for (let i = 0; i < 3; i++) {
      await user.click(await screen.findByRole("button", { name: /^next$/i }));
    }
    await screen.findByRole("button", { name: /launch run/i });

    // The composed policy carries ONLY the git_pat grant — never an api_key
    // grant that would misroute the PAT to api.anthropic.com. "git_pat"
    // legitimately appears twice (the Grants chip + the raw policy JSON
    // below it) — getAllByText tolerates that; "api_key" must appear NOWHERE.
    expect(screen.getAllByText("git_pat").length).toBeGreaterThan(0);
    expect(screen.queryAllByText("api_key")).toHaveLength(0);
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

  // N2: stepError used to only disable Next/Review-now — validateStep's
  // message was never rendered anywhere, so a blocked step was a silent grey
  // button. It must now be visible without even needing a click.
  it("renders the blocked-step reason inline instead of a silent disabled Next (N2)", async () => {
    render(
      <PermissionWizard
        open
        onOpenChange={() => {}}
        onCreated={() => {}}
        initialState={{ ...initialWizardState("CC2"), mode: "batch", task: "" }}
      />,
    );
    const nextBtn = await screen.findByRole("button", { name: /^next$/i });
    expect(nextBtn).toBeDisabled();
    const banner = await screen.findByTestId("wizard-step-error");
    expect(banner).toHaveTextContent("An autonomous run needs a task to perform.");
    // The banned wire word (copy.ts) must never appear now that this string
    // actually renders.
    expect(banner).not.toHaveTextContent(/batch/i);
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
  // The console could always WRITE policies (save-as-policy here, the Policies
  // screen) but never RUN one: buildSpec unconditionally emitted an inline_policy,
  // so policy_id was permanently blank for console-launched runs. Picking a saved
  // policy on Basics must launch it BY REFERENCE (policy_id XOR inline_policy) so
  // the server enforces the STORED spec — and any hand edit must detach, because
  // buildSpec normalizes and does not round-trip a stored spec.
  const savedPolicy = {
    id: "pol-1",
    name: "payments-strict",
    created_at: "now",
    updated_at: "now",
    spec: {
      allowed_domains: ["api.anthropic.com"],
      first_use_approval: "always_deny" as const,
      min_confinement_class: "CC2" as const,
    },
  };

  it("launches under a saved policy by reference (policy_id, no inline_policy)", async () => {
    listPoliciesMock.mockResolvedValue([savedPolicy]);
    createRunMock.mockResolvedValue(createdRun);
    render(
      <PermissionWizard open onOpenChange={() => {}} onCreated={() => {}} initialState={readyState()} />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    // Picking it on Basics unlocks the same "Review now" fast-track a recorded
    // profile does, and Review shows the STORED spec, not a composed one.
    await user.click(await screen.findByRole("radio", { name: /payments-strict/ }));
    await user.click(await screen.findByRole("button", { name: /review now/i }));
    expect(await screen.findByText(/sent by reference as policy_id/i)).toBeInTheDocument();

    await user.click(await screen.findByRole("button", { name: /launch run/i }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalledTimes(1));
    expect(createRunMock.mock.calls[0][0]).toMatchObject({ policy_id: "pol-1" });
    expect(createRunMock.mock.calls[0][0].inline_policy).toBeUndefined();
  });

  // W15-W15e-wizard-roundtrip-1: workspace_mounts/workspace_repos live ONLY on
  // inline_policy (RunPolicySpec) — attaching a saved policy drops
  // inline_policy entirely (XOR with policy_id), so the workspace picked on
  // Access silently never reaches the server and the run launches on an
  // empty scratch dir. workspace_id is a SEPARATE top-level field
  // (seedRequestWorkspace applies it AFTER resolving the stored policy) and
  // must ride along even when policy_id is set.
  it("still conveys the picked workspace via workspace_id when launching under a saved policy (W15-W15e-wizard-roundtrip-1)", async () => {
    listPoliciesMock.mockResolvedValue([savedPolicy]);
    createRunMock.mockResolvedValue(createdRun);
    render(
      <PermissionWizard open onOpenChange={() => {}} onCreated={() => {}} initialState={readyState()} />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("radio", { name: /payments-strict/ }));
    await user.click(await screen.findByRole("button", { name: /review now/i }));
    await user.click(await screen.findByRole("button", { name: /launch run/i }));

    await waitFor(() => expect(createRunMock).toHaveBeenCalledTimes(1));
    expect(createRunMock.mock.calls[0][0]).toMatchObject({ policy_id: "pol-1", workspace_id: workspace.id });
  });

  // D11/claim6: a recorded PROFILE sets selectedProfile but (unlike a saved
  // policy) never selectedPolicyId, so the old detach branch — keyed only off
  // selectedPolicyId — never fired for it. After a hand edit, Basics kept
  // claiming the profile and Review kept hiding "Save as a reusable policy"
  // on the stated rationale "re-saving the same spec is redundant" — which is
  // exactly what it no longer was.
  it("detaches a recorded PROFILE back to inline as soon as a step is edited (D11)", async () => {
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

    // Applying the profile is ITSELF a patch() call carrying selectedProfile —
    // it must not immediately self-detach.
    const profile = await screen.findByTestId("basics-profile-build-test");
    await user.click(within(profile).getByRole("radio"));
    await waitFor(() => expect(profileRunMock).toHaveBeenCalledWith("rec-1"));

    await user.click(await screen.findByRole("button", { name: /review now/i }));
    // Attached: re-saving the same spec is redundant, so the switch is hidden.
    expect(screen.queryByText("Save as a reusable policy")).toBeNull();

    // Jump back and hand-edit Egress — the run must no longer claim to be
    // that recording.
    await user.click(screen.getByRole("button", { name: /egress/i }));
    await user.click(screen.getByRole("switch", { name: /allow all egress/i }));

    // Basics stops claiming the profile...
    await user.click(screen.getByRole("button", { name: /basics/i }));
    expect(screen.getByRole("radio", { name: /configure manually/i })).toBeChecked();

    // ...and Review's save-as-policy switch un-hides.
    await goToLastStep(user);
    expect(await screen.findByText("Save as a reusable policy")).toBeInTheDocument();
  });

  // Review F1: the detach exemption must be PER-FIELD, not per-patch. A patch
  // that names selectedProfile alone (applying a recorded profile, a Basics
  // workspace change) must still clear a live selectedPolicyId — the per-patch
  // exemption left the stale policy id behind while every UI signal read
  // "detached", and launch sent policy_id: the STORED spec, its workspace
  // mounts included, instead of the inline one on screen.
  it("switching from a saved policy to a recorded profile drops the stale policy_id (F1)", async () => {
    const wsWithRec: Workspace = {
      ...workspace,
      record_results: {
        "build-test": { run_id: "rec-1", label: "build & test", mode: "interactive", status: "recorded" },
      },
    };
    listWorkspacesMock.mockReset();
    listWorkspacesMock.mockResolvedValue([wsWithRec]);
    listPoliciesMock.mockResolvedValue([savedPolicy]);
    profileRunMock.mockResolvedValue({
      proposed: { inline_policy: { min_confinement_class: "CC2", allowed_domains: ["registry.npmjs.org"] } },
    });
    createRunMock.mockResolvedValue(createdRun);
    render(
      <PermissionWizard open onOpenChange={() => {}} onCreated={() => {}} initialState={readyState()} />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("radio", { name: /payments-strict/ }));
    const profile = await screen.findByTestId("basics-profile-build-test");
    await user.click(within(profile).getByRole("radio"));
    await waitFor(() => expect(profileRunMock).toHaveBeenCalledWith("rec-1"));

    await user.click(await screen.findByRole("button", { name: /review now/i }));
    // The recording's spec, sent verbatim — never the abandoned policy's
    // stored spec by reference.
    expect(screen.queryByText(/sent by reference as policy_id/i)).toBeNull();
    await user.click(await screen.findByRole("button", { name: /launch run/i }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalledTimes(1));
    expect(createRunMock.mock.calls[0][0].policy_id).toBeUndefined();
    expect(createRunMock.mock.calls[0][0].inline_policy).toMatchObject({
      allowed_domains: expect.arrayContaining(["registry.npmjs.org"]),
    });
  });

  it("detaches back to an inline policy as soon as a step is edited", async () => {
    listPoliciesMock.mockResolvedValue([savedPolicy]);
    createRunMock.mockResolvedValue(createdRun);
    render(
      <PermissionWizard open onOpenChange={() => {}} onCreated={() => {}} initialState={readyState()} />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("radio", { name: /payments-strict/ }));
    await user.click(await screen.findByRole("button", { name: /review now/i }));
    // Jump back and widen egress — the run must no longer claim to be that policy.
    await user.click(screen.getByRole("button", { name: /egress/i }));
    await user.click(screen.getByRole("switch", { name: /allow all egress/i }));
    await user.click(await screen.findByRole("button", { name: /^next$/i }));
    await user.click(await screen.findByRole("button", { name: /^next$/i }));

    await user.click(await screen.findByRole("button", { name: /launch run/i }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalledTimes(1));
    expect(createRunMock.mock.calls[0][0].policy_id).toBeUndefined();
    expect(createRunMock.mock.calls[0][0].inline_policy).toMatchObject({ allow_all_egress: true });
  });

  // Stage 3: the New-run dialog's workspace-first pick seeds a FRESH wizard
  // entry (no initialState — that's only "Edit in wizard") via a dedicated
  // initialWorkspaces prop, specifically so it does NOT short-circuit the
  // confinement-class auto-resolution the way a full initialState intentionally
  // does.
  it("initialWorkspaces seeds Basics' selection on a fresh entry without a full initialState", async () => {
    render(
      <PermissionWizard
        open
        onOpenChange={() => {}}
        onCreated={() => {}}
        initialWorkspaces={[{ workspaceId: workspace.id }]}
      />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    // Basics already shows the seeded workspace as the primary — no combobox
    // interaction needed, same card the manual multi-select would produce.
    expect(await screen.findByText("acme-repo", { exact: true })).toBeInTheDocument();
    expect(screen.getByText("primary", { exact: true })).toBeInTheDocument();

    // The health probe still ran and resolved a real tier set (proving the
    // "fresh entry" resolution path wasn't suppressed by initialWorkspaces the
    // way a full initialState intentionally suppresses it): Confinement renders
    // a real barrier choice, not stuck on "Checking…".
    await goToConfinementStep(user);
    expect(screen.queryByText(/checking…/i)).not.toBeInTheDocument();
  });

  // N1: the manual wizard used to have no risk grade — and so no ack gate — at
  // all, so "Edit in wizard" from a HIGH-graded composer proposal silently
  // walked the operator around the acknowledgment compose-review.tsx enforces
  // for the identical spec. Preflight now carries risk_assessment/overall_risk
  // (preflight.go/preflight_test.go) and StepReview renders the SAME RiskPanel
  // (its own rendering rules are covered by step-review.test.tsx); this block
  // covers the WIZARD's half — gating Launch, and never letting a stale
  // acknowledgment survive a fresh preflight.
  const highRiskPreflight = {
    setup_items: [],
    enforced_confinement_class: "CC2",
    risk_assessment: [
      {
        field: "allow_all_egress",
        value: "true",
        risk_level: "high",
        rationale: "Reaches almost any public host.",
      },
    ],
    overall_risk: "high",
  };

  it("gates Launch behind the acknowledgment checkbox when preflight grades HIGH (N1)", async () => {
    preflightRunMock.mockResolvedValue(highRiskPreflight);
    createRunMock.mockResolvedValue(createdRun);
    render(
      <PermissionWizard open onOpenChange={() => {}} onCreated={() => {}} initialState={readyState()} />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await goToLastStep(user);
    const launchBtn = await screen.findByRole("button", { name: /launch run/i });
    await waitFor(() => expect(launchBtn).toBeDisabled());
    expect(screen.getByTestId("high-risk-section")).toBeInTheDocument();

    await user.click(screen.getByRole("checkbox"));
    await waitFor(() => expect(launchBtn).toBeEnabled());

    await user.click(launchBtn);
    await waitFor(() => expect(createRunMock).toHaveBeenCalledTimes(1));
  });

  it("resets the acknowledgment when a fresh preflight lands, so a stale ack can't survive re-entering Review (N1)", async () => {
    preflightRunMock.mockResolvedValue(highRiskPreflight);
    render(
      <PermissionWizard open onOpenChange={() => {}} onCreated={() => {}} initialState={readyState()} />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await goToLastStep(user);
    const launchBtn = await screen.findByRole("button", { name: /launch run/i });
    await waitFor(() => expect(launchBtn).toBeDisabled());
    await user.click(screen.getByRole("checkbox"));
    await waitFor(() => expect(launchBtn).toBeEnabled());

    // Leave Review and come back — a fresh preflight is requested, and the
    // prior acknowledgment must not carry over even though the mocked
    // response is byte-identical: the reset fires on "a new preflight was
    // asked for" (runPreflight), not on detecting a diff.
    await user.click(screen.getByRole("button", { name: /back/i }));
    await user.click(await screen.findByRole("button", { name: /^next$/i }));
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.getByRole("button", { name: /launch run/i })).toBeDisabled());
  });

  it("shows no ack gate and Launch stays enabled when preflight carries no risk_assessment (advisory degrade)", async () => {
    // beforeEach's default preflightRunMock response predates risk_assessment
    // entirely (an older-server shape) — Launch must behave exactly as before
    // this batch: no panel, no gate, never blocked by a preflight that has
    // nothing to say about risk.
    createRunMock.mockResolvedValue(createdRun);
    render(
      <PermissionWizard open onOpenChange={() => {}} onCreated={() => {}} initialState={readyState()} />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await goToLastStep(user);
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalled());
    expect(await screen.findByRole("button", { name: /launch run/i })).toBeEnabled();
    expect(screen.queryByTestId("high-risk-section")).toBeNull();
  });
});

// ui-newrun-1: every close path (Cancel, Escape, an overlay click, the header
// X) must funnel through ONE requestClose() that only prompts when `state`
// has diverged from the open-time snapshot. The sharpest regression case is
// the PRIOR fix attempt's own bug: the snapshot must be taken from the
// SEEDED initial state (initialWorkspaces/initialInteractive/initialState),
// never a pre-seed baseline — otherwise an untouched Cancel on a workspace-
// first pre-seeded wizard spuriously prompts.
describe("PermissionWizard — unsaved-work close guard (ui-newrun-1)", () => {
  beforeEach(() => {
    healthMock.mockReset().mockResolvedValue({ confinement_classes: ["CC1", "CC2", "CC3"] });
    listSecretsMock.mockReset().mockResolvedValue([]);
    listWorkspacesMock.mockReset().mockResolvedValue([workspace]);
    listPoliciesMock.mockReset().mockResolvedValue([]);
    preflightRunMock.mockReset();
  });

  it("Cancel on a freshly-opened, untouched wizard closes with no prompt", async () => {
    const onOpenChange = vi.fn();
    render(<PermissionWizard open onOpenChange={onOpenChange} onCreated={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await screen.findByRole("button", { name: /^next$/i });
    await user.click(screen.getByRole("button", { name: /^cancel$/i }));

    expect(screen.queryByText(/discard this run/i)).not.toBeInTheDocument();
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  // The prior fix attempt's bug, pinned: a workspace-first pre-seed (the
  // New-run dialog's own flow, via initialWorkspaces — no full initialState)
  // must read as the OPEN-time baseline, not a diff against it.
  it("Cancel on a workspace-first PRE-SEEDED (but untouched) wizard closes with no prompt", async () => {
    const onOpenChange = vi.fn();
    render(
      <PermissionWizard
        open
        onOpenChange={onOpenChange}
        onCreated={() => {}}
        initialWorkspaces={[{ workspaceId: workspace.id }]}
      />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    // Let the seed (and the async post-health-probe reseed) settle.
    await screen.findByText("acme-repo", { exact: true });
    await user.click(screen.getByRole("button", { name: /^cancel$/i }));

    expect(screen.queryByText(/discard this run/i)).not.toBeInTheDocument();
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("Cancel after a real edit prompts to discard; Keep editing stays open, Discard closes", async () => {
    const onOpenChange = vi.fn();
    render(<PermissionWizard open onOpenChange={onOpenChange} onCreated={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    // Basics -> Access -> Egress, then a real edit.
    await user.click(await screen.findByRole("button", { name: /^next$/i }));
    await user.click(await screen.findByRole("button", { name: /^next$/i }));
    await user.click(await screen.findByRole("switch", { name: /allow all egress/i }));

    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(await screen.findByText(/discard this run/i)).toBeInTheDocument();
    expect(onOpenChange).not.toHaveBeenCalled();

    // Keep editing dismisses the prompt without closing.
    await user.click(screen.getByRole("button", { name: /keep editing/i }));
    expect(screen.queryByText(/discard this run/i)).not.toBeInTheDocument();
    expect(onOpenChange).not.toHaveBeenCalled();

    // Cancel again and actually discard.
    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
    await user.click(screen.getByRole("button", { name: /^discard$/i }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("Escape on a dirty wizard routes through the same guard, not just Cancel", async () => {
    const onOpenChange = vi.fn();
    render(<PermissionWizard open onOpenChange={onOpenChange} onCreated={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /^next$/i }));
    await user.click(await screen.findByRole("button", { name: /^next$/i }));
    await user.click(await screen.findByRole("switch", { name: /allow all egress/i }));

    fireEvent.keyDown(document, { key: "Escape" });

    expect(await screen.findByText(/discard this run/i)).toBeInTheDocument();
    expect(onOpenChange).not.toHaveBeenCalled();
  });
});
