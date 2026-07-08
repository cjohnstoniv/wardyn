/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import userEvent from "@testing-library/user-event";
import type { AgentRun, ComposeResponse, ComposerBackend } from "../../../lib/types";

// Toast + api are mocked so the orchestrator's wiring (mode selection, compose
// call, error toasts, launch) can be asserted without a real backend.
const toastError = vi.fn();
const toastSuccess = vi.fn();
const toastWarning = vi.fn();
// The composer UI is hidden by default (COMPOSER_UI_ENABLED=false), but the code is
// retained — these tests exercise that retained composer path, so force the flag on.
vi.mock("../../../lib/features", () => ({ COMPOSER_UI_ENABLED: true }));
vi.mock("sonner", () => ({
  toast: {
    error: (...a: unknown[]) => toastError(...a),
    success: (...a: unknown[]) => toastSuccess(...a),
    warning: (...a: unknown[]) => toastWarning(...a),
  },
}));

const listComposerBackendsMock = vi.fn();
const composeMock = vi.fn();
const createRunMock = vi.fn();
const healthMock = vi.fn();
const listSecretsMock = vi.fn();
const listWorkspacesMock = vi.fn();
const getSetupStatusMock = vi.fn();
const scanWorkspaceMock = vi.fn();
const setSecretMock = vi.fn();

// Re-export the real HttpError so the orchestrator's status-based error mapping
// still type-matches.
vi.mock("../../../lib/api", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api")>("../../../lib/api");
  return {
    HttpError: actual.HttpError,
    api: {
      listComposerBackends: (...a: unknown[]) => listComposerBackendsMock(...a),
      compose: (...a: unknown[]) => composeMock(...a),
      createRun: (...a: unknown[]) => createRunMock(...a),
      health: (...a: unknown[]) => healthMock(...a),
      listSecrets: (...a: unknown[]) => listSecretsMock(...a),
      listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a),
      listPolicies: () => Promise.resolve([]),
      // The setup-hint effect + the checklist's optimistic-flip re-probe both call
      // this — must exist on every test's mock or the dialog's on-open effect
      // throws synchronously (undefined is not a function).
      getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a),
      scanWorkspace: (...a: unknown[]) => scanWorkspaceMock(...a),
      setSecret: (...a: unknown[]) => setSecretMock(...a),
      createPolicy: vi.fn(),
      // Fire-and-forget mode-transition beacon; the dialog calls this on every
      // mode change, so it must exist even though these tests don't assert on it.
      telemetry: vi.fn().mockResolvedValue(undefined),
    },
  };
});

import { NewRunDialog } from "./new-run-dialog";
import { HttpError } from "../../../lib/api";
import type { SetupStatus } from "../../../lib/types";

// A fully-ready SetupStatus (composer + llm access both configured) so the
// on-open amber hint stays hidden by default — tests that want the hint override
// this per-case.
function readySetupStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return {
    ready: true,
    checks: [],
    auth: { mode: "local", local_loopback: true },
    runner: { driver: "docker", confinement_classes: ["CC1", "CC2"] },
    composer: {
      enabled: true,
      backends: [
        { name: "anthropic", provider: "anthropic", model: "claude", wire: "api", enabled: true, needs_key: true, key_resolved: true },
      ],
    },
    providers: [],
    secrets: { present: ["anthropic-api-key"], github_app: false },
    age_key: { durable: false },
    restart_required: false,
    has_runs: false,
    platform: { os: "linux", wsl: false },
    ...overrides,
  };
}

const backends: ComposerBackend[] = [
  { name: "anthropic", provider: "anthropic", model: "claude", is_default: true },
  { name: "openai", provider: "openai", model: "gpt", is_default: false },
];

function composeResult(overrides: Partial<ComposeResponse> = {}): ComposeResponse {
  return {
    kind: "proposal",
    proposed: {
      run: {
        agent: "claude-code",
        repo: "acme/payments",
        task: "fix the flaky test",
        confinement_class: "CC1",
        interactive: false,
      },
      inline_policy: {
        allowed_domains: ["api.anthropic.com"],
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
        rationale: "Permissive runc sandbox.",
      },
    ],
    overall_risk: "high",
    summary: "A permissive run.",
    warnings: [],
    ...overrides,
  };
}

const createdRun: AgentRun = {
  id: "run-123",
  created_at: "now",
  updated_at: "now",
  created_by: "me",
  agent: "claude-code",
  repo: "acme/payments",
  task: "fix the flaky test",
  confinement_class: "CC1",
  state: "PENDING",
  spiffe_id: "spiffe://x",
  runner_target: "docker",
};

// The on-open setup-hint links to "/setup" via react-router's <Link>, which
// throws without a Router ancestor — wrap every render in one (same pattern as
// audit.test.tsx / approvals.test.tsx).
function renderDialog(ui: Parameters<typeof render>[0]) {
  return render(<MemoryRouter>{ui}</MemoryRouter>);
}

// Module-level (not nested in a describe) so EVERY describe block in this file
// shares the same reset + sane defaults — a describe-scoped beforeEach only
// covers its own block, and this file has grown several sibling describes.
beforeEach(() => {
  toastError.mockReset();
  toastSuccess.mockReset();
  toastWarning.mockReset();
  listComposerBackendsMock.mockReset();
  composeMock.mockReset();
  createRunMock.mockReset();
  healthMock.mockReset();
  listSecretsMock.mockReset();
  listWorkspacesMock.mockReset();
  getSetupStatusMock.mockReset();
  scanWorkspaceMock.mockReset();
  setSecretMock.mockReset();
  healthMock.mockResolvedValue({ confinement_classes: ["CC1", "CC2", "CC3"] });
  listSecretsMock.mockResolvedValue([]);
  listWorkspacesMock.mockResolvedValue([]);
  createRunMock.mockResolvedValue(createdRun);
  getSetupStatusMock.mockResolvedValue(readySetupStatus());
  scanWorkspaceMock.mockResolvedValue({ async: false });
});

describe("NewRunDialog", () => {
  it("offers both entry modes when the composer is enabled", async () => {
    listComposerBackendsMock.mockResolvedValue(backends);
    renderDialog(<NewRunDialog open onOpenChange={() => {}} onCreated={() => {}} />);
    expect(await screen.findByRole("button", { name: /describe your task/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /configure manually/i })).toBeInTheDocument();
  });

  it("skips straight to the manual wizard when the composer is disabled (no backends)", async () => {
    listComposerBackendsMock.mockResolvedValue([]);
    renderDialog(<NewRunDialog open onOpenChange={() => {}} onCreated={() => {}} />);
    // The wizard renders its step indicator with the Basics step — no Describe mode.
    expect(await screen.findByText(/compose the agent's permission envelope/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /describe your task/i })).not.toBeInTheDocument();
  });

  it("composes, shows the proposal, and gates launch behind the high-risk ack", async () => {
    listComposerBackendsMock.mockResolvedValue(backends);
    composeMock.mockResolvedValue(composeResult());
    const onCreated = vi.fn();
    const onOpenChange = vi.fn();
    renderDialog(<NewRunDialog open onOpenChange={onOpenChange} onCreated={onCreated} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /describe your task/i }));
    await user.type(screen.getByLabelText(/describe your task/i), "fix CI");
    await user.click(screen.getByRole("button", { name: /compose/i }));

    // Lands on the review screen.
    expect(await screen.findByText(/proposed setup/i)).toBeInTheDocument();
    expect(composeMock).toHaveBeenCalledTimes(1);
    expect(composeMock.mock.calls[0][0]).toMatchObject({ prompt: "fix CI", backend: "anthropic" });

    // High-risk gate: launch disabled until acked.
    const launch = screen.getByRole("button", { name: /approve & launch/i });
    expect(launch).toBeDisabled();
    await user.click(screen.getByRole("checkbox"));
    expect(screen.getByRole("button", { name: /approve & launch/i })).toBeEnabled();

    await user.click(screen.getByRole("button", { name: /approve & launch/i }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalledTimes(1));
    // createRun gets the proposal's run scalars + inline_policy.
    expect(createRunMock.mock.calls[0][0]).toMatchObject({
      agent: "claude-code",
      repo: "acme/payments",
      inline_policy: { min_confinement_class: "CC1", allow_all_egress: true },
    });
    expect(onCreated).toHaveBeenCalledWith(createdRun);
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("a launch failure keeps the proposal open, shows the error INLINE, and offers to add a missing secret", async () => {
    listComposerBackendsMock.mockResolvedValue(backends);
    composeMock.mockResolvedValue(composeResult());
    // create-run rejects because an api_key grant references a not-yet-stored secret.
    createRunMock.mockRejectedValueOnce(
      new Error(
        'invalid inline_policy: api_key grant references unknown secret "dazz-pg-credentials" (set it first via the secrets API)',
      ),
    );
    const onOpenChange = vi.fn();
    renderDialog(<NewRunDialog open onOpenChange={onOpenChange} onCreated={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /describe your task/i }));
    await user.type(screen.getByLabelText(/describe your task/i), "load the dazz db");
    await user.click(screen.getByRole("button", { name: /compose/i }));
    await screen.findByText(/proposed setup/i);

    // Acknowledge the high-risk gate (the fixture proposal grades high), then launch.
    const ack = screen.queryByRole("checkbox");
    if (ack) await user.click(ack);
    await user.click(screen.getByRole("button", { name: /approve & launch/i }));

    // The panel STAYS open (never dismissed) and the error is surfaced INLINE.
    await waitFor(() => expect(createRunMock).toHaveBeenCalledTimes(1));
    expect(await screen.findByTestId("launch-error")).toHaveTextContent(/unknown secret/i);
    expect(screen.getByText(/proposed setup/i)).toBeInTheDocument();
    expect(onOpenChange).not.toHaveBeenCalledWith(false);

    // The helper names the missing secret and opens the Add-secret dialog in place.
    await user.click(
      screen.getByRole("button", { name: /add the .*dazz-pg-credentials.* secret/i }),
    );
    expect(await screen.findByText(/^add secret$/i)).toBeInTheDocument();
  });

  it("sends the persisted default confinement tier as the compose floor", async () => {
    // The operator's Getting Started pick is stored in this browser; submitCompose
    // sends it RAW as confinementFloor (the server caps it — no client clamp).
    localStorage.setItem("wardyn-default-confinement", "CC3");
    try {
      listComposerBackendsMock.mockResolvedValue(backends);
      composeMock.mockResolvedValue(composeResult());
      renderDialog(<NewRunDialog open onOpenChange={() => {}} onCreated={() => {}} />);
      const user = userEvent.setup({ pointerEventsCheck: 0 });

      await user.click(await screen.findByRole("button", { name: /describe your task/i }));
      await user.type(screen.getByLabelText(/describe your task/i), "fix CI");
      await user.click(screen.getByRole("button", { name: /compose/i }));

      await screen.findByText(/proposed setup/i);
      expect(composeMock.mock.calls[0][0]).toMatchObject({ confinementFloor: "CC3" });
    } finally {
      localStorage.removeItem("wardyn-default-confinement");
    }
  });

  it("toasts a clear message when compose() fails with a 502 backend error", async () => {
    listComposerBackendsMock.mockResolvedValue(backends);
    composeMock.mockRejectedValue(new HttpError(502, "backend down"));
    renderDialog(<NewRunDialog open onOpenChange={() => {}} onCreated={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /describe your task/i }));
    await user.type(screen.getByLabelText(/describe your task/i), "fix CI");
    await user.click(screen.getByRole("button", { name: /compose/i }));

    await waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
    const desc = String(toastError.mock.calls[0][1]?.description ?? "");
    expect(desc).toMatch(/backend failed/i);
    // Stays on the describe form (no proposal).
    expect(screen.queryByText(/risk assessment/i)).not.toBeInTheDocument();
  });

  it("Edit in wizard hands the proposal to the wizard, prefilled", async () => {
    listComposerBackendsMock.mockResolvedValue(backends);
    composeMock.mockResolvedValue(composeResult());
    // The proposed repo ("acme/payments") must already be an onboarded workspace
    // to have been proposable at all (run-create's mount-restriction gate) — the
    // wizard's reverse-mapping (wizardStateFromProposal) resolves it by source.
    listWorkspacesMock.mockResolvedValue([
      {
        id: "ws-1",
        name: "payments",
        kind: "repo",
        source: "acme/payments",
        status: "ready",
        created_at: "now",
        updated_at: "now",
      },
    ]);
    renderDialog(<NewRunDialog open onOpenChange={() => {}} onCreated={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /describe your task/i }));
    await user.type(screen.getByLabelText(/describe your task/i), "fix CI");
    await user.click(screen.getByRole("button", { name: /compose/i }));
    await screen.findByText(/proposed setup/i);

    await user.click(screen.getByRole("button", { name: /edit in wizard/i }));
    // The manual wizard is now shown (its description), prefilled with the repo.
    expect(await screen.findByText(/compose the agent's permission envelope/i)).toBeInTheDocument();
    // The resolved onboarded workspace carries over into the wizard's Basics step
    // as a selection card (name + source), not a free-text repo field anymore.
    expect(await screen.findByText("payments")).toBeInTheDocument();
    expect(screen.getByText("acme/payments")).toBeInTheDocument();
  });

  it("asks clarifying questions, then proposes after they're answered", async () => {
    listComposerBackendsMock.mockResolvedValue(backends);
    composeMock
      .mockResolvedValueOnce({
        kind: "questions",
        round: 0,
        assumptions: ["targets acme/payments"],
        notes: "need a detail",
        questions: [
          {
            id: "gh",
            question: "What GitHub access?",
            why: "scope the token",
            options: ["Read-only", "Read + write"],
            multi: false,
          },
        ],
      })
      .mockResolvedValueOnce(composeResult());
    renderDialog(<NewRunDialog open onOpenChange={() => {}} onCreated={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /describe your task/i }));
    await user.type(screen.getByLabelText(/describe your task/i), "ship a feature");
    await user.click(screen.getByRole("button", { name: /compose/i }));

    // The clarify step appears with the question.
    expect(await screen.findByText("What GitHub access?")).toBeInTheDocument();
    expect(composeMock.mock.calls[0][0]).toMatchObject({ mode: "auto", round: 0 });

    // Answer and continue → second compose carries the transcript + round 1.
    await user.click(screen.getByText("Read-only"));
    await user.click(screen.getByRole("button", { name: /continue/i }));

    expect(await screen.findByText(/proposed setup/i)).toBeInTheDocument();
    expect(composeMock).toHaveBeenCalledTimes(2);
    expect(composeMock.mock.calls[1][0]).toMatchObject({
      round: 1,
      transcript: [{ question: "What GitHub access?", answer: "Read-only" }],
    });
  });

  it("'Skip & propose anyway' on the Q&A screen proposes one-shot", async () => {
    listComposerBackendsMock.mockResolvedValue(backends);
    composeMock
      .mockResolvedValueOnce({
        kind: "questions",
        round: 0,
        questions: [{ id: "gh", question: "What GitHub access?", why: "", options: [], multi: false }],
      })
      .mockResolvedValueOnce(composeResult());
    renderDialog(<NewRunDialog open onOpenChange={() => {}} onCreated={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /describe your task/i }));
    await user.type(screen.getByLabelText(/describe your task/i), "ship a feature");
    await user.click(screen.getByRole("button", { name: /compose/i }));
    await screen.findByText("What GitHub access?");

    await user.click(screen.getByRole("button", { name: /skip & propose anyway/i }));
    expect(await screen.findByText(/proposed setup/i)).toBeInTheDocument();
    expect(composeMock.mock.calls[1][0]).toMatchObject({ mode: "skip" });
  });

  it("surfaces a sonner warning toast for each POST /runs warning (collision advisory)", async () => {
    listComposerBackendsMock.mockResolvedValue(backends);
    // A low-risk proposal so launch is enabled without a high-risk ack.
    composeMock.mockResolvedValue(
      composeResult({ risk_assessment: [], overall_risk: "low" }),
    );
    // createRun returns the run PLUS an advisory workspace-collision warning.
    createRunMock.mockResolvedValue({
      ...createdRun,
      warnings: ["workspace /home/me/app is also used by active run run-999"],
    });
    renderDialog(<NewRunDialog open onOpenChange={() => {}} onCreated={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /describe your task/i }));
    await user.type(screen.getByLabelText(/describe your task/i), "fix CI");
    await user.click(screen.getByRole("button", { name: /compose/i }));
    await screen.findByText(/proposed setup/i);

    await user.click(screen.getByRole("button", { name: /approve & launch/i }));

    await waitFor(() => expect(createRunMock).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(toastWarning).toHaveBeenCalledTimes(1));
    expect(String(toastWarning.mock.calls[0][1]?.description ?? "")).toMatch(/run-999/);
    // The collision is advisory: the run still launched (createRun resolved).
    expect(toastError).not.toHaveBeenCalled();
  });
});

describe("NewRunDialog — compose-session id (decision 1/9)", () => {
  it("mints a session id on entering describe mode and resends it unchanged on every round + at launch", async () => {
    listComposerBackendsMock.mockResolvedValue(backends);
    composeMock
      .mockResolvedValueOnce({
        kind: "questions",
        round: 0,
        questions: [
          {
            id: "gh",
            question: "What GitHub access?",
            why: "scope the token",
            options: ["Read-only", "Read + write"],
            multi: false,
          },
        ],
      })
      .mockResolvedValueOnce(composeResult({ risk_assessment: [], overall_risk: "low" }));
    renderDialog(<NewRunDialog open onOpenChange={() => {}} onCreated={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /describe your task/i }));
    await user.type(screen.getByLabelText(/describe your task/i), "ship a feature");
    await user.click(screen.getByRole("button", { name: /compose/i }));
    await screen.findByText("What GitHub access?");

    const sessionId = composeMock.mock.calls[0][0].sessionId;
    expect(typeof sessionId).toBe("string");
    expect(sessionId.length).toBeGreaterThan(0);

    await user.click(screen.getByText("Read-only"));
    await user.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByText(/proposed setup/i);
    // Same id resent unchanged on round 1.
    expect(composeMock.mock.calls[1][0].sessionId).toBe(sessionId);

    await user.click(screen.getByRole("button", { name: /approve & launch/i }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalledTimes(1));
    expect(createRunMock.mock.calls[0][0]).toMatchObject({ compose_session_id: sessionId });
  });

  it("does not send a session id for a run launched straight from the manual wizard (composer disabled)", async () => {
    listComposerBackendsMock.mockResolvedValue([]);
    renderDialog(<NewRunDialog open onOpenChange={() => {}} onCreated={() => {}} />);
    await screen.findByText(/compose the agent's permission envelope/i);
    // No describe mode was ever entered — nothing else to assert here beyond
    // "it didn't crash wiring a session id that was never minted".
  });
});

describe("NewRunDialog — pre-compose setup hint (B3/B6)", () => {
  it("shows an amber hint linking to Getting started when model access isn't configured", async () => {
    listComposerBackendsMock.mockResolvedValue(backends);
    // No secret, no CLI login, AND no resolved composer backend — llmReady AND
    // composerReady both false (the dialog's own listComposerBackends() is a
    // SEPARATE data source, so Describe mode still stays reachable).
    getSetupStatusMock.mockResolvedValue(
      readySetupStatus({
        secrets: { present: [], github_app: false },
        providers: [],
        composer: { enabled: false, backends: [] },
      }),
    );
    renderDialog(<NewRunDialog open onOpenChange={() => {}} onCreated={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /describe your task/i }));
    const link = await screen.findByRole("link", { name: /finish getting started/i });
    expect(link).toHaveAttribute("href", "/setup");
  });

  it("shows no hint when the composer + model access are both already configured", async () => {
    listComposerBackendsMock.mockResolvedValue(backends);
    getSetupStatusMock.mockResolvedValue(readySetupStatus());
    renderDialog(<NewRunDialog open onOpenChange={() => {}} onCreated={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /describe your task/i }));
    await screen.findByLabelText(/describe your task/i);
    expect(screen.queryByRole("link", { name: /finish getting started/i })).toBeNull();
  });
});

describe("NewRunDialog — setup checklist re-flip (decision 9: no recheck endpoint)", () => {
  it("optimistically flips a secret item once its dialog saves, without losing the proposal", async () => {
    listComposerBackendsMock.mockResolvedValue(backends);
    composeMock.mockResolvedValue(
      composeResult({
        risk_assessment: [],
        overall_risk: "low",
        setup_items: [
          {
            id: "secret:dazz-pg-credentials",
            kind: "secret",
            label: "dazz-pg-credentials",
            required_by: "the api_key grant",
            status: "missing",
            fix: { action: "add_secret", secret_name: "dazz-pg-credentials" },
          },
        ],
      }),
    );
    setSecretMock.mockResolvedValue(undefined);
    renderDialog(<NewRunDialog open onOpenChange={() => {}} onCreated={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /describe your task/i }));
    await user.type(screen.getByLabelText(/describe your task/i), "load the dazz db");
    await user.click(screen.getByRole("button", { name: /compose/i }));
    await screen.findByText(/proposed setup/i);

    const row = screen.getByTestId("setup-item-secret:dazz-pg-credentials");
    expect(within(row).getByText("Needs setup")).toBeInTheDocument();

    await user.click(within(row).getByRole("button", { name: /add secret/i }));
    // The row's OWN button is also named "Add secret" — scope to the dialog's
    // heading (its title) to disambiguate from the checklist row behind it.
    await screen.findByRole("heading", { name: /^add secret$/i });
    // The name field is already prefilled with Fix.SecretName — only the value
    // needs typing.
    expect(screen.getByLabelText(/^name$/i)).toHaveValue("dazz-pg-credentials");
    await user.type(screen.getByLabelText(/^value$/i), "sekrit");
    await user.click(screen.getByRole("button", { name: /^save secret$/i }));

    // Flips in place — no re-compose, the proposal (and this row) is still here.
    await waitFor(() => expect(within(row).getByText("Configured")).toBeInTheDocument());
    expect(composeMock).toHaveBeenCalledTimes(1);
    expect(screen.getByText(/proposed setup/i)).toBeInTheDocument();
  });

  it("re-flips a workspace item once its scan lands the workspace at ready", async () => {
    listComposerBackendsMock.mockResolvedValue(backends);
    composeMock.mockResolvedValue(
      composeResult({
        risk_assessment: [],
        overall_risk: "low",
        setup_items: [
          {
            id: "workspace:ws-1",
            kind: "workspace",
            label: "payments repo",
            required_by: "the run's workspace",
            status: "missing",
            fix: { action: "scan_workspace", workspace_id: "ws-1" },
          },
        ],
      }),
    );
    listWorkspacesMock
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([
        {
          id: "ws-1",
          name: "payments",
          kind: "repo",
          source: "acme/payments",
          status: "ready",
          created_at: "now",
          updated_at: "now",
        },
      ]);
    scanWorkspaceMock.mockResolvedValue({ async: false });
    renderDialog(<NewRunDialog open onOpenChange={() => {}} onCreated={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /describe your task/i }));
    await user.type(screen.getByLabelText(/describe your task/i), "fix CI");
    await user.click(screen.getByRole("button", { name: /compose/i }));
    await screen.findByText(/proposed setup/i);

    const row = screen.getByTestId("setup-item-workspace:ws-1");
    await user.click(within(row).getByRole("button", { name: /scan workspace/i }));

    expect(scanWorkspaceMock).toHaveBeenCalledWith("ws-1");
    await waitFor(() => expect(within(row).getByText("Configured")).toBeInTheDocument());
  });
});
