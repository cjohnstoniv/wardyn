/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// HarnessDemoStep — locked/live states, the D2 preflight gate, and the D2/
// operator write-gate on Start, in isolation from the rest of the funnel
// (setup-screen.test.tsx covers only the orchestrator-owned wiring: the
// Integrations CTA and the launched-override).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const createRunMock = vi.fn();
const getRunMock = vi.fn();
const killRunMock = vi.fn();
const preflightRunMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    createRun: (...a: unknown[]) => createRunMock(...a),
    getRun: (...a: unknown[]) => getRunMock(...a),
    killRun: (...a: unknown[]) => killRunMock(...a),
    preflightRun: (...a: unknown[]) => preflightRunMock(...a),
  },
}));
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: ({ runId }: { runId: string }) => <div data-testid="attach-terminal">{runId}</div>,
}));
vi.mock("../../wardyn/live-approvals", () => ({
  LiveApprovals: () => <div data-testid="live-approvals" />,
}));
const listAuditMock = vi.fn();
vi.mock("../../../lib/api/audit", () => ({
  audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) },
  egressFromAudit: () => [],
}));

import HarnessDemoStep from "./harness-demo-step";
import { DEMOS } from "../demos/demo-catalog";
import { OperatorProvider } from "../../wardyn/operator-context";
import { baseStatus } from "./test-fixtures";

const HARNESS = DEMOS.find((d) => d.id === "agent-in-the-box")!;
const SATISFIED_ITEM = {
  id: "llm_access:claude-code",
  kind: "llm_access",
  label: "Model access for claude-code",
  required_by: "the agent's own model calls",
  status: "satisfied",
};
const MISSING_ITEM = { ...SATISFIED_ITEM, status: "missing", detail: "no credential resolves" };

function renderStep(props: Partial<Parameters<typeof HarnessDemoStep>[0]> = {}, operator = true) {
  return render(
    <OperatorProvider operator={operator}>
      <HarnessDemoStep
        status={baseStatus()}
        barrierReady={true}
        onJump={vi.fn()}
        onDemoLaunched={vi.fn()}
        {...props}
      />
    </OperatorProvider>,
  );
}

// useDemoRuns' own mount effect (reload re-attach) and DemoAuditPanel poll —
// both fire regardless of locked/live state once DemoRunControls/StepList
// mount — so these need real resolved values on every test, same as
// demo-screen.test.tsx's own beforeEach.
beforeEach(() => {
  localStorage.clear();
  vi.clearAllMocks();
  getRunMock.mockResolvedValue(undefined);
  killRunMock.mockResolvedValue(undefined);
  listAuditMock.mockResolvedValue([]);
});

describe("HarnessDemoStep — locked (no agent-capable AI integration)", () => {

  it("shows the invitation panel, not an error: no Start button anywhere", async () => {
    renderStep({ status: baseStatus() });
    expect(await screen.findByText("Connect a model provider first")).toBeInTheDocument();
    expect(
      screen.getByText("A real agent run against your model provider, with egress sealed to that provider alone."),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Go to Integrations" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /start demo/i })).not.toBeInTheDocument();
    // Never fires a preflight while locked — nothing to check yet.
    expect(preflightRunMock).not.toHaveBeenCalled();
  });

  it("'Go to Integrations' navigates via onJump, never a raw link", async () => {
    const onJump = vi.fn();
    renderStep({ status: baseStatus(), onJump });
    await screen.findByText("Connect a model provider first");
    await userEvent.setup().click(screen.getByRole("button", { name: "Go to Integrations" }));
    expect(onJump).toHaveBeenCalledWith("integrations");
  });
});

describe("HarnessDemoStep — live (an agent-capable AI integration resolves)", () => {
  const liveStatus = baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } });

  beforeEach(() => {
    vi.clearAllMocks();
    preflightRunMock.mockResolvedValue({ setup_items: [SATISFIED_ITEM], enforced_confinement_class: "CC1" });
    createRunMock.mockResolvedValue({ id: "harness-run-1", state: "RUNNING" });
  });

  it("composes the catalog's demo body verbatim: overview, step list, no caution (this demo has none)", async () => {
    renderStep({ status: liveStatus });
    expect(await screen.findByTestId(`demo-start-${HARNESS.id}`)).toBeInTheDocument();
    expect(screen.getByText(HARNESS.overview)).toBeInTheDocument();
    for (const step of HARNESS.steps) {
      expect(screen.getByText(step.text)).toBeInTheDocument();
    }
    expect(screen.queryByTestId("demo-caution")).not.toBeInTheDocument();
  });

  it("preflights the exact create body, renders the checklist, and enables Start once satisfied", async () => {
    renderStep({ status: liveStatus });
    await waitFor(() =>
      expect(preflightRunMock).toHaveBeenCalledWith({
        agent: "claude-code",
        interactive: true,
        inline_policy: HARNESS.policy,
        integration_id: "ai:anthropic_api_key",
      }),
    );
    expect(await screen.findByText("Model access for claude-code")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByTestId(`demo-start-${HARNESS.id}`)).toBeEnabled());
  });

  it("Start launches with the SAME body preflight checked (D1 overlay, mirrored from demo-screen's launch)", async () => {
    renderStep({ status: liveStatus });
    const start = await screen.findByTestId(`demo-start-${HARNESS.id}`);
    await waitFor(() => expect(start).toBeEnabled());
    await userEvent.setup({ pointerEventsCheck: 0 }).click(start);
    expect(createRunMock).toHaveBeenCalledWith({
      agent: "claude-code",
      interactive: true,
      inline_policy: HARNESS.policy,
      integration_id: "ai:anthropic_api_key",
    });
  });

  it("marks the demo launched (per-browser signal) once Start succeeds", async () => {
    const onDemoLaunched = vi.fn();
    renderStep({ status: liveStatus, onDemoLaunched });
    const start = await screen.findByTestId(`demo-start-${HARNESS.id}`);
    await waitFor(() => expect(start).toBeEnabled());
    await userEvent.setup({ pointerEventsCheck: 0 }).click(start);
    await waitFor(() => expect(onDemoLaunched).toHaveBeenCalledWith("agent-in-the-box"));
  });

  it("switches to codex-cli (and the OpenAI integration id) when the bound row is an OpenAI key", async () => {
    const openaiStatus = baseStatus({ secrets: { present: ["openai-api-key"], github_app: false } });
    renderStep({ status: openaiStatus });
    await waitFor(() =>
      expect(preflightRunMock).toHaveBeenCalledWith(
        expect.objectContaining({ agent: "codex-cli", integration_id: "ai:openai_api_key" }),
      ),
    );
  });

  it("blocks Start when preflight says model access is missing — other rows stay advisory (render, don't block)", async () => {
    preflightRunMock.mockResolvedValue({
      setup_items: [MISSING_ITEM, { id: "backend:cc1", kind: "backend", label: "Sandbox runner", required_by: "the run itself", status: "missing" }],
      enforced_confinement_class: "CC1",
    });
    renderStep({ status: liveStatus });
    expect(await screen.findByText("Model access for claude-code")).toBeInTheDocument();
    expect(screen.getByText("Sandbox runner")).toBeInTheDocument(); // the OTHER row still renders
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalled());
    expect(screen.getByTestId(`demo-start-${HARNESS.id}`)).toBeDisabled();
  });

  it("a preflight transport error is advisory: Start stays enabled with a neutral note, not blocked", async () => {
    preflightRunMock.mockRejectedValue(new Error("network down"));
    renderStep({ status: liveStatus });
    expect(await screen.findByTestId("harness-preflight-unavailable")).toHaveTextContent(/preflight unavailable/i);
    await waitFor(() => expect(screen.getByTestId(`demo-start-${HARNESS.id}`)).toBeEnabled());
  });

  it("shows the not-ready banner and keeps Start disabled without a sandbox barrier", async () => {
    renderStep({ status: liveStatus, barrierReady: false });
    expect(await screen.findByTestId("harness-step-not-ready")).toBeInTheDocument();
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalled());
    expect(screen.getByTestId(`demo-start-${HARNESS.id}`)).toBeDisabled();
  });

  // Admin-scoped write: a viewer sees the SAME honest live state (a model really
  // is connected — never re-shown as the locked/"not connected" panel) but can't
  // launch it, exactly like every other write control in this funnel (operator ==
  // admin here; a later lane renames role semantics).
  it("a non-operator sees the live body (never the locked panel) but Start stays disabled", async () => {
    renderStep({ status: liveStatus }, /* operator */ false);
    expect(await screen.findByText(HARNESS.overview)).toBeInTheDocument();
    expect(screen.queryByText("Connect a model provider first")).not.toBeInTheDocument();
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalled());
    expect(screen.getByTestId(`demo-start-${HARNESS.id}`)).toBeDisabled();
  });
});
