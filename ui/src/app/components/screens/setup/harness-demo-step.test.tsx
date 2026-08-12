/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// HarnessDemoStep — locked (none / openai_only) / live states, and the D2
// preflight gate, in isolation from the rest of the funnel
// (setup-screen.test.tsx covers only the orchestrator-owned wiring: the
// Integrations CTA and the launched-override). Launching is viewer-tier — no
// operator gate here, matching the other four demo steps.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
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
import { baseStatus } from "./test-fixtures";

const HARNESS = DEMOS.find((d) => d.id === "agent-in-the-box")!;
const EXPECTED_BODY = { agent: "claude-code", interactive: true, inline_policy: HARNESS.policy };
const SATISFIED_ITEM = {
  id: "llm_access:claude-code",
  kind: "llm_access",
  label: "Model access for claude-code",
  required_by: "the agent's own model calls",
  status: "satisfied",
};
const MISSING_ITEM = { ...SATISFIED_ITEM, status: "missing", detail: "no credential resolves" };

function renderStep(props: Partial<Parameters<typeof HarnessDemoStep>[0]> = {}) {
  return render(
    <HarnessDemoStep
      status={baseStatus()}
      barrierReady={true}
      onJump={vi.fn()}
      onDemoLaunched={vi.fn()}
      {...props}
    />,
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
  it("shows the invitation panel, not an error: no Start button, no OpenAI-only note", async () => {
    renderStep({ status: baseStatus() });
    expect(await screen.findByText("Connect a model provider first")).toBeInTheDocument();
    expect(
      screen.getByText("A real agent run against your model provider, with egress sealed to that provider alone."),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Go to Integrations" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /start demo/i })).not.toBeInTheDocument();
    expect(screen.queryByText(/connect an anthropic-capable provider/i)).not.toBeInTheDocument();
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

  // Codex CLI isn't supported here (the catalog's task/policy are Claude-
  // only, and deploy/images/codex-cli has no claude binary) — an OpenAI-only
  // deployment stays locked, with an honest reason, not a silent Codex swap.
  it("stays locked for an OpenAI-only integration, with the honest reason", async () => {
    const openaiStatus = baseStatus({ secrets: { present: ["openai-api-key"], github_app: false } });
    renderStep({ status: openaiStatus });
    expect(await screen.findByText("Connect a model provider first")).toBeInTheDocument();
    expect(
      screen.getByText("This demo runs Claude Code — connect an Anthropic-capable provider to try it."),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /start demo/i })).not.toBeInTheDocument();
    expect(preflightRunMock).not.toHaveBeenCalled();
  });
});

describe("HarnessDemoStep — live (a Claude-Code-capable AI integration resolves)", () => {
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

  it("preflights the exact create body (claude-code, no integration_id override), renders the checklist, and enables Start once satisfied", async () => {
    renderStep({ status: liveStatus });
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalledWith(EXPECTED_BODY));
    expect(await screen.findByText("Model access for claude-code")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByTestId(`demo-start-${HARNESS.id}`)).toBeEnabled());
  });

  it("Start launches with the SAME body preflight checked — mirrored from demo-screen's launch, never forked", async () => {
    renderStep({ status: liveStatus });
    const start = await screen.findByTestId(`demo-start-${HARNESS.id}`);
    await waitFor(() => expect(start).toBeEnabled());
    await userEvent.setup({ pointerEventsCheck: 0 }).click(start);
    expect(createRunMock).toHaveBeenCalledWith(EXPECTED_BODY);
  });

  it("marks the demo launched (per-browser signal) once Start succeeds", async () => {
    const onDemoLaunched = vi.fn();
    renderStep({ status: liveStatus, onDemoLaunched });
    const start = await screen.findByTestId(`demo-start-${HARNESS.id}`);
    await waitFor(() => expect(start).toBeEnabled());
    await userEvent.setup({ pointerEventsCheck: 0 }).click(start);
    await waitFor(() => expect(onDemoLaunched).toHaveBeenCalledWith("agent-in-the-box"));
  });

  it("also goes live for an OpenAI key CO-connected alongside an Anthropic one (prefers Claude Code)", async () => {
    const bothStatus = baseStatus({ secrets: { present: ["anthropic-api-key", "openai-api-key"], github_app: false } });
    renderStep({ status: bothStatus });
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalledWith(EXPECTED_BODY));
  });

  // M2 fix: an ABSENT llm_access row must fail CLOSED, not open — the old
  // `!!llmItem && …` let a resolved-but-silent preflight through.
  it("fails CLOSED when preflight resolves with no llm_access row at all", async () => {
    preflightRunMock.mockResolvedValue({ setup_items: [], enforced_confinement_class: "CC1" });
    renderStep({ status: liveStatus });
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalled());
    expect(screen.getByTestId(`demo-start-${HARNESS.id}`)).toBeDisabled();
  });

  // H1 fix: a definite unsatisfied row surfaces its detail AND an escape —
  // a dead Start with no forward path is a dead end.
  it("blocks Start when preflight says model access is missing, surfaces the detail, and offers the same 'Go to Integrations' escape", async () => {
    preflightRunMock.mockResolvedValue({
      setup_items: [MISSING_ITEM, { id: "backend:cc1", kind: "backend", label: "Sandbox runner", required_by: "the run itself", status: "missing" }],
      enforced_confinement_class: "CC1",
    });
    const onJump = vi.fn();
    renderStep({ status: liveStatus, onJump });
    expect(await screen.findByText("Model access for claude-code")).toBeInTheDocument();
    expect(screen.getByText("Sandbox runner")).toBeInTheDocument(); // the OTHER row still renders, advisory
    await waitFor(() => expect(screen.getByTestId(`demo-start-${HARNESS.id}`)).toBeDisabled());

    const escape = screen.getByTestId("harness-llm-access-blocked");
    expect(escape).toHaveTextContent("no credential resolves"); // the row's own detail
    await userEvent.setup().click(within(escape).getByRole("button", { name: "Go to Integrations" }));
    expect(onJump).toHaveBeenCalledWith("integrations");
  });

  it("a preflight transport error is advisory: Start stays enabled with a neutral note, not blocked", async () => {
    preflightRunMock.mockRejectedValue(new Error("network down"));
    renderStep({ status: liveStatus });
    expect(await screen.findByTestId("harness-preflight-unavailable")).toHaveTextContent(
      "Preflight unavailable — you can still start.",
    );
    await waitFor(() => expect(screen.getByTestId(`demo-start-${HARNESS.id}`)).toBeEnabled());
  });

  it("shows the not-ready banner and keeps Start disabled without a sandbox barrier", async () => {
    renderStep({ status: liveStatus, barrierReady: false });
    expect(await screen.findByTestId("harness-step-not-ready")).toBeInTheDocument();
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalled());
    expect(screen.getByTestId(`demo-start-${HARNESS.id}`)).toBeDisabled();
  });
});
