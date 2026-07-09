/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SetupStatus, Workspace, WorkspaceProfile } from "../../../lib/types";

// The Import panel is driven entirely off api + the workspace status, so the api
// is mocked and each test hands in a workspace at a known status to assert the
// rail resumes on the right step and each pane renders the right actions.
const getWorkspaceMock = vi.fn();
const listWorkspacesMock = vi.fn();
const listSecretsMock = vi.fn();
const scanWorkspaceMock = vi.fn();
const setSetupCommandsMock = vi.fn();
const verifyWorkspaceMock = vi.fn();
const recordTaskMock = vi.fn();
const getObservedEgressMock = vi.fn();
const setApprovedEgressMock = vi.fn();
const finalizeWorkspaceMock = vi.fn();
const listComposerBackendsMock = vi.fn();
const suggestVerifyFixMock = vi.fn();
const getSetupStatusMock = vi.fn();

// The AI-diagnose affordance is composer-gated + hidden by default
// (COMPOSER_UI_ENABLED=false); force the flag on so its retained code stays covered.
vi.mock("../../../lib/features", () => ({ COMPOSER_UI_ENABLED: true }));
vi.mock("../../../lib/api", () => ({
  api: {
    getWorkspace: (...a: unknown[]) => getWorkspaceMock(...a),
    listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a),
    listSecrets: (...a: unknown[]) => listSecretsMock(...a),
    scanWorkspace: (...a: unknown[]) => scanWorkspaceMock(...a),
    setSetupCommands: (...a: unknown[]) => setSetupCommandsMock(...a),
    verifyWorkspace: (...a: unknown[]) => verifyWorkspaceMock(...a),
    recordTask: (...a: unknown[]) => recordTaskMock(...a),
    getObservedEgress: (...a: unknown[]) => getObservedEgressMock(...a),
    setApprovedEgress: (...a: unknown[]) => setApprovedEgressMock(...a),
    finalizeWorkspace: (...a: unknown[]) => finalizeWorkspaceMock(...a),
    listComposerBackends: (...a: unknown[]) => listComposerBackendsMock(...a),
    suggestVerifyFix: (...a: unknown[]) => suggestVerifyFixMock(...a),
    getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a),
    // Referenced by the embedded Add* dialogs only when opened; present so a
    // stray effect never throws "undefined is not a function".
    createWorkspace: vi.fn(),
    updateWorkspace: vi.fn(),
    setSecret: vi.fn(),
  },
}));
vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() },
}));

import { ImportWorkspaceDialog } from "./import-panel";

function ws(over: Partial<Workspace> = {}, profile: WorkspaceProfile = {}): Workspace {
  return {
    id: "ws-1",
    name: "payments",
    kind: "repo",
    source: "acme/payments",
    status: "scanned",
    created_at: "",
    updated_at: "",
    profile: profile as unknown as Record<string, unknown>,
    ...over,
  };
}

// Minimal SetupStatus fixture (mirrors setup-screen.test.tsx's baseStatus) — no
// LLM path by default, so RecordPane's model-readiness warning renders unless a
// test opts a provider/secret/composer backend in via overrides.
function setupStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return {
    ready: false,
    checks: [],
    auth: { mode: "local", local_loopback: true },
    runner: { driver: "docker", confinement_classes: ["CC1", "CC2"] },
    composer: { enabled: false, backends: [] },
    providers: [{ tool: "claude", installed: true, logged_in: false }],
    secrets: { present: [], github_app: false },
    age_key: { durable: false },
    restart_required: false,
    has_runs: false,
    platform: { os: "linux", wsl: false, kvm: true },
    ...overrides,
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  listWorkspacesMock.mockResolvedValue([]);
  listSecretsMock.mockResolvedValue([]);
  getObservedEgressMock.mockResolvedValue({ denied: [], runs_examined: 0 });
  // Composer OFF by default so the deterministic-flow tests above don't render the
  // AI affordance; the AI-diagnosis suite below opts in per test.
  listComposerBackendsMock.mockResolvedValue([]);
  // M18: no LLM path by default (see setupStatus()); the M18 suite below opts a
  // provider in to prove the warning is driven by /setup/status, not composer.
  getSetupStatusMock.mockResolvedValue(setupStatus());
});

// A verify_failed workspace, ready for the Verify pane's fix affordances.
function failedWs() {
  return ws({
    status: "verify_failed",
    verify_result: { ran: true, ok: false, steps: [{ stage: "build", command: "npm ci", exit_code: 1 }] },
  });
}

describe("ImportWorkspaceDialog — rail resumes on the workspace's status", () => {
  it("renders all five rail steps and resumes a scanned workspace on Configure", async () => {
    getWorkspaceMock.mockResolvedValue(
      ws({ status: "scanned" }, { required_secrets: [{ name: "DATABASE_URL", kind: "postgres" }] }),
    );
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);

    // The rail shows every step.
    for (const label of ["Source", "Scan", "Configure", "Verify", "Finalize"]) {
      expect(await screen.findByText(label)).toBeInTheDocument();
    }
    // Configure pane content: security chip + setup commands + declared secret.
    expect(await screen.findByText(/secrets are brokered/i)).toBeInTheDocument();
    expect(screen.getByTestId("setup-commands")).toBeInTheDocument();
    expect(screen.getByText("DATABASE_URL")).toBeInTheDocument();
  });

  it("resumes a verify_failed workspace on the Verify step", async () => {
    getWorkspaceMock.mockResolvedValue(
      ws({
        status: "verify_failed",
        verify_result: { ran: true, ok: false, steps: [{ stage: "build", command: "npm ci", exit_code: 1 }] },
      }),
    );
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);

    expect(await screen.findByText(/environment verification/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /re-run verify/i })).toBeInTheDocument();
  });
});

describe("ImportWorkspaceDialog — verify failure renders steps + masked logs", () => {
  it("shows each step's stage, exit code, and the log_head/log_tail exactly as given", async () => {
    // Logs arrive already MASKED from the server — the panel must render them
    // verbatim (never unmask), so we hand it masked text and assert it appears.
    getWorkspaceMock.mockResolvedValue(
      ws({
        status: "verify_failed",
        verify_result: {
          ran: true,
          ok: false,
          steps: [
            {
              stage: "build",
              command: "npm run build",
              exit_code: 1,
              log_head: "npm ERR! network request failed",
              log_tail: "AUTH=****MASKED**** build aborted",
            },
          ],
        },
      }),
    );
    const { container } = render(
      <ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />,
    );

    const steps = await screen.findByTestId("verify-steps");
    expect(within(steps).getByText("npm run build")).toBeInTheDocument();
    expect(within(steps).getByText(/exit 1/)).toBeInTheDocument();
    expect(within(steps).getByText(/npm ERR! network request failed/)).toBeInTheDocument();
    expect(within(steps).getByText(/AUTH=\*\*\*\*MASKED\*\*\*\* build aborted/)).toBeInTheDocument();
    // Nothing unmasked leaked into the DOM.
    expect(container.textContent).not.toMatch(/sk[-_]live[-_]|AKIA[0-9A-Z]{16}/);
  });
});

describe("ImportWorkspaceDialog — live verify progress (streamed)", () => {
  it("shows the running step, its streamed log tail, done/pending rows, and step N of total", async () => {
    getWorkspaceMock.mockResolvedValue(
      ws({
        status: "verifying",
        setup_commands: [
          { stage: "install", command: "npm ci" },
          { stage: "build", command: "npm run build" },
          { stage: "test", command: "npm test" },
        ],
        verify_result: {
          ran: true,
          ok: false,
          done: false,
          total: 3,
          steps: [
            { stage: "install", command: "npm ci", exit_code: 0, duration_ms: 12000 },
            {
              stage: "build",
              command: "npm run build",
              running: true,
              exit_code: -1,
              log_tail: "webpack: compiling… TOKEN=****MASKED****",
            },
          ],
        },
      }),
    );
    const { container } = render(
      <ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />,
    );

    // Progress header: two of three steps have started.
    expect(await screen.findByText(/step 2 of 3/i)).toBeInTheDocument();

    const steps = screen.getByTestId("verify-steps");
    // Running step: gerund label + its streamed (already-masked) log tail, verbatim.
    expect(within(steps).getByText(/building…/i)).toBeInTheDocument();
    expect(within(steps).getByText(/webpack: compiling… TOKEN=\*\*\*\*MASKED\*\*\*\*/)).toBeInTheDocument();
    // Done step shows its duration; not-yet-started step shows as waiting.
    expect(within(steps).getByText("12s")).toBeInTheDocument();
    expect(within(steps).getByText("npm test")).toBeInTheDocument();
    expect(within(steps).getByText("waiting")).toBeInTheDocument();
    // The masked log is rendered as given — nothing unmasked leaks.
    expect(container.textContent).not.toMatch(/sk[-_]live[-_]|AKIA[0-9A-Z]{16}/);
  });

  it("does not crash on a degraded verify_result missing total/running/done", async () => {
    getWorkspaceMock.mockResolvedValue(
      ws({
        status: "verifying",
        setup_commands: [{ stage: "build", command: "npm run build" }],
        verify_result: {
          ran: true,
          ok: false,
          steps: [{ stage: "build", command: "npm run build", exit_code: 0 }],
        },
      }),
    );
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);

    // Still shows a progress header + the step, inferring total from the command list.
    expect(await screen.findByTestId("verify-progress")).toHaveTextContent(/step 1 of 1/i);
    expect(within(screen.getByTestId("verify-steps")).getByText("npm run build")).toBeInTheDocument();
  });

  it("renders a bare 'Verifying…' header when verify_result is absent", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "verifying" }));
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);

    expect(await screen.findByTestId("verify-progress")).toHaveTextContent(/verifying environment/i);
    expect(screen.queryByTestId("verify-steps")).not.toBeInTheDocument();
  });
});

describe("ImportWorkspaceDialog — verify with no runner (503) is honest", () => {
  it("renders the honest no-runner message + a finalize-anyway path", async () => {
    getWorkspaceMock.mockResolvedValue(
      ws({
        status: "scanned",
        record_results: {
          "build-test": { run_id: "o1", label: "build & test", mode: "interactive", status: "recorded" },
        },
      }),
    );
    recordTaskMock.mockResolvedValue({ ok: false, status: 503, detail: "no runner configured" });
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    // scanned resumes on Configure; step forward to Verify, then replay the recording.
    await screen.findByText(/secrets are brokered/i);
    await user.click(screen.getByRole("button", { name: /next: record/i }));
    await user.click(await screen.findByRole("button", { name: /continue to verify/i }));
    await user.click(await screen.findByRole("button", { name: /verify this recording/i }));

    // Honest no-runner message + a still-usable path to Finalize (footer).
    expect(await screen.findByTestId("record-no-runner")).toHaveTextContent(/runner none/i);
    expect(screen.getByRole("button", { name: /continue to finalize/i })).toBeInTheDocument();
    expect(recordTaskMock).toHaveBeenCalledWith("ws-1", "build & test", true);
  });
});

describe("ImportWorkspaceDialog — finalize", () => {
  it("passes emitEnvAsCode:true and shows the emitted files, then closes on Done", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "ready" }));
    finalizeWorkspaceMock.mockResolvedValue({
      workspace: ws({ status: "ready" }),
      emitted_files: { "devcontainer.json": '{\n  "name": "payments"\n}' },
    });
    const onOpenChange = vi.fn();
    const onReload = vi.fn();
    render(
      <ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={onOpenChange} onReload={onReload} />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    // ready resumes on Finalize.
    await user.click(await screen.findByRole("checkbox"));
    await user.click(screen.getByRole("button", { name: /finalize import/i }));

    await waitFor(() =>
      expect(finalizeWorkspaceMock).toHaveBeenCalledWith("ws-1", { emitEnvAsCode: true }),
    );
    // The returned file content is shown in a copyable block.
    expect(await screen.findByText("devcontainer.json")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /^done$/i }));
    expect(onReload).toHaveBeenCalledTimes(1);
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("closes immediately (reload + onOpenChange) when no files are emitted", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "ready" }));
    finalizeWorkspaceMock.mockResolvedValue({ workspace: ws({ status: "ready" }), emitted_files: {} });
    const onOpenChange = vi.fn();
    const onReload = vi.fn();
    render(
      <ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={onOpenChange} onReload={onReload} />,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /finalize import/i }));
    await waitFor(() =>
      expect(finalizeWorkspaceMock).toHaveBeenCalledWith("ws-1", { emitEnvAsCode: false }),
    );
    expect(onReload).toHaveBeenCalledTimes(1);
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});

describe("ImportWorkspaceDialog — Record step nav (Configure → Record → Verify)", () => {
  it("steps Configure → Record, then Continue to Verify lands on the Verify pane", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "scanned" }));
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    // scanned resumes on Configure.
    await screen.findByText(/secrets are brokered/i);
    await user.click(screen.getByRole("button", { name: /next: record/i }));

    // Record pane is shown (recommended/skippable, never routes away).
    expect(await screen.findByText(/record sessions/i)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /continue to verify/i }));
    // Verify replays a RECORDING confined — with none recorded yet, it points back
    // to Record (no free-text session naming here).
    expect(await screen.findByTestId("verify-no-recordings")).toBeInTheDocument();
  });

  it("Skip recording also lands on Verify (recording is never required)", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "scanned" }));
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await screen.findByText(/secrets are brokered/i);
    await user.click(screen.getByRole("button", { name: /next: record/i }));
    await user.click(await screen.findByRole("button", { name: /skip recording/i }));
    expect(await screen.findByTestId("verify-no-recordings")).toBeInTheDocument();
  });
});

describe("ImportWorkspaceDialog — configure saves approved setup commands", () => {
  it("PUTs the included, edited commands via setSetupCommands", async () => {
    getWorkspaceMock.mockResolvedValue(
      ws({ status: "scanned" }, { setup_commands: [{ stage: "install", command: "npm ci", source: "detected" }] }),
    );
    setSetupCommandsMock.mockResolvedValue(ws({ status: "scanned" }));
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    const card = await screen.findByTestId("setup-commands");
    await user.click(within(card).getByRole("button", { name: /save setup commands/i }));

    await waitFor(() =>
      expect(setSetupCommandsMock).toHaveBeenCalledWith("ws-1", [
        { stage: "install", command: "npm ci", source: "detected" },
      ]),
    );
  });

  // Regression: "Add command" used to seed stage:"setup", a value the server
  // rejects (valid stages are install|build|test|lint) — a silent 400 on save.
  // The default must be a valid stage, and the operator must be able to pick
  // any of the four via the stage dropdown.
  it("Add command defaults to a valid stage, and the dropdown can change it", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "scanned" }));
    setSetupCommandsMock.mockResolvedValue(ws({ status: "scanned" }));
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    const card = await screen.findByTestId("setup-commands");
    await user.click(within(card).getByRole("button", { name: /add command/i }));

    const stageSelect = within(card).getByRole("combobox") as HTMLSelectElement;
    expect(stageSelect.value).toBe("install");

    await user.selectOptions(stageSelect, "test");
    await user.type(within(card).getByPlaceholderText("npm ci"), "npm test");
    await user.click(within(card).getByRole("button", { name: /save setup commands/i }));

    await waitFor(() =>
      expect(setSetupCommandsMock).toHaveBeenCalledWith("ws-1", [
        { stage: "test", command: "npm test", source: "operator" },
      ]),
    );
  });
});

describe("ImportWorkspaceDialog — agentic verify diagnosis (AI)", () => {
  it("renders the suggestion after an explicit click when a composer is configured", async () => {
    getWorkspaceMock.mockResolvedValue(failedWs());
    listComposerBackendsMock.mockResolvedValue([{ name: "anthropic", is_default: true }]);
    suggestVerifyFixMock.mockResolvedValue("Allow egress to registry.npmjs.org, then re-run verify.");
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    // Affordance is present but nothing is requested until the operator clicks (human-gated).
    const btn = await screen.findByRole("button", { name: /diagnose with ai/i });
    expect(suggestVerifyFixMock).not.toHaveBeenCalled();

    await user.click(btn);
    expect(await screen.findByTestId("verify-ai-suggestion")).toHaveTextContent(/registry\.npmjs\.org/);
    expect(suggestVerifyFixMock).toHaveBeenCalledWith("ws-1");
  });

  it("shows an error state (and no suggestion) when the backend call fails", async () => {
    getWorkspaceMock.mockResolvedValue(failedWs());
    listComposerBackendsMock.mockResolvedValue([{ name: "anthropic", is_default: true }]);
    suggestVerifyFixMock.mockRejectedValue(new Error("backend unavailable"));
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /diagnose with ai/i }));
    expect(await screen.findByTestId("verify-ai-error")).toHaveTextContent(/backend unavailable/i);
    expect(screen.queryByTestId("verify-ai-suggestion")).not.toBeInTheDocument();
  });

  it("hides the AI affordance entirely when no composer is configured", async () => {
    getWorkspaceMock.mockResolvedValue(failedWs());
    listComposerBackendsMock.mockResolvedValue([]); // composer off
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);

    // The deterministic egress fix is still offered; the AI diagnosis is not.
    expect(await screen.findByTestId("verify-fix")).toBeInTheDocument();
    expect(screen.queryByTestId("verify-ai-fix")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /diagnose with ai/i })).not.toBeInTheDocument();
  });
});

// M17: the Verify pane's one-click "approve a denied host" fix used to PUT
// straight to setApprovedEgress — skipping the same untrusted-content confirm
// the Workspaces screen enforces for the identical action (the host name came
// from a run's observed/denied egress, not something the operator typed).
describe("ImportWorkspaceDialog — egress approvals confirm before applying (M17)", () => {
  it("does not call setApprovedEgress until the confirm dialog is accepted", async () => {
    getWorkspaceMock.mockResolvedValue(failedWs());
    getObservedEgressMock.mockResolvedValue({ denied: ["evil.example.com"], runs_examined: 3 });
    setApprovedEgressMock.mockResolvedValue(failedWs());
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /suggest a fix from denied egress/i }));
    await user.click(await screen.findByRole("button", { name: /^approve$/i }));

    // The confirm dialog names the host and blocks the write until accepted.
    expect(await screen.findByText(/approve egress to evil\.example\.com/i)).toBeInTheDocument();
    expect(setApprovedEgressMock).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /approve host/i }));
    await waitFor(() => expect(setApprovedEgressMock).toHaveBeenCalledWith("ws-1", ["evil.example.com"]));
  });

  it("cancelling the confirm dialog leaves the host unapproved", async () => {
    getWorkspaceMock.mockResolvedValue(failedWs());
    getObservedEgressMock.mockResolvedValue({ denied: ["evil.example.com"], runs_examined: 3 });
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /suggest a fix from denied egress/i }));
    await user.click(await screen.findByRole("button", { name: /^approve$/i }));
    await screen.findByText(/approve egress to evil\.example\.com/i);

    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(setApprovedEgressMock).not.toHaveBeenCalled();
  });
});

// M18: Record's "no model configured" warning used to be wired to composer
// detection (force-disabled by COMPOSER_UI_ENABLED=false in prod), so it fired
// even with a connected subscription or API key. It must instead reflect GET
// /setup/status (hasLlmPath) — independent of the composer signal.
describe("ImportWorkspaceDialog — Record model-readiness reflects /setup/status, not composer (M18)", () => {
  it("shows the connected note when a provider is logged in, even with no composer backend", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "scanned" }));
    getSetupStatusMock.mockResolvedValue(
      setupStatus({ providers: [{ tool: "claude", installed: true, logged_in: true }] }),
    );
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /next: record/i }));
    expect(await screen.findByText(/configured model provider/i)).toBeInTheDocument();
    expect(screen.queryByText(/no model provider is configured/i)).not.toBeInTheDocument();
  });

  it("still warns when /setup/status reports no provider, key, or composer backend", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "scanned" }));
    // default setupStatus() (from beforeEach) has no logged-in provider/secret.
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /next: record/i }));
    expect(await screen.findByText(/no model provider is configured/i)).toBeInTheDocument();
  });
});
