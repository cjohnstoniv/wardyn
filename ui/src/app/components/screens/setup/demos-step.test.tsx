/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Was demos/demo-screen.test.tsx, against the /demos grid. That page is gone —
// DemoDetail is the single demo renderer now — so the same behaviours are
// exercised through it: one demo per render, `barrierReady` as a prop instead
// of a setup-status fetch of its own.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

// Mock the api client so createRun/getRun are asserted and no network happens.
const createRunMock = vi.fn();
const getRunMock = vi.fn();
const killRunMock = vi.fn();
const getGrantsMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    createRun: (...a: unknown[]) => createRunMock(...a),
    getRun: (...a: unknown[]) => getRunMock(...a),
    killRun: (...a: unknown[]) => killRunMock(...a),
    getGrants: (...a: unknown[]) => getGrantsMock(...a),
  },
}));

// AttachTerminal drags in xterm + a live WebSocket; LiveApprovals polls the API.
// Stub both to inert markers so the step composition is what's under test.
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: ({ runId }: { runId: string }) => <div data-testid="attach-terminal">{runId}</div>,
}));
vi.mock("../../wardyn/live-approvals", () => ({
  LiveApprovals: ({ runId }: { runId: string }) => <div data-testid="live-approvals">{runId}</div>,
}));
// The inline audit panel polls /audit; stub it to no decisions (empty projection).
const listAuditMock = vi.fn();
vi.mock("../../../lib/api/audit", () => ({
  audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) },
  egressFromAudit: () => [],
  demoAuditRows: () => [],
}));
// "Turn this into a policy" opens ProfileReview, which POSTs /runs/{id}/profile.
vi.mock("../profile-review", () => ({
  ProfileReview: ({ runId }: { runId: string | null }) =>
    runId ? <div data-testid="profile-review">{runId}</div> : null,
}));
const toastErrorMock = vi.fn();
vi.mock("sonner", () => ({ toast: { error: (...a: unknown[]) => toastErrorMock(...a) } }));

import DemoDetail from "./demos-step";
import { DEMOS, type Demo } from "../demos/demo-catalog";
import { HttpError } from "../../../lib/api/core";
import { OperatorProvider } from "../../wardyn/operator-context";

const FIRST = DEMOS[0];
const APP_GATED = DEMOS.find((d) => d.needsGitHubApp)!;
const REFUSED_AT_CREATE = DEMOS.find((d) => d.id === "sts-fail-closed")!;

function renderDemo(
  demo: Demo = FIRST,
  barrierReady = true,
  opts: { githubAppReady?: boolean; operator?: boolean; onDemoLaunched?: () => void } = {},
) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator={opts.operator ?? true}>
        <DemoDetail
          demo={demo}
          barrierReady={barrierReady}
          githubAppReady={opts.githubAppReady ?? true}
          onJump={vi.fn()}
          onDemoLaunched={opts.onDemoLaunched ?? vi.fn()}
        />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

describe("DemoDetail — the single demo renderer", () => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });
  beforeEach(() => {
    localStorage.clear();
    createRunMock.mockReset().mockResolvedValue({ id: "demo-run-1", state: "RUNNING" });
    getRunMock.mockReset().mockResolvedValue({ id: "demo-run-1", state: "RUNNING" });
    killRunMock.mockReset().mockResolvedValue(undefined);
    getGrantsMock.mockReset().mockResolvedValue([]);
    listAuditMock.mockReset().mockResolvedValue([]);
    toastErrorMock.mockReset();
  });

  // H4: `demo-card-<id>` existed only on the dead /demos page's DemoCard. It is
  // what keeps "the episode changes URL only" true — the selector has to survive
  // the move onto the funnel step (funnel.ts documents this exact trap).
  it("carries the demo-card-<id> wrapper testid the /demos cards used to own", async () => {
    renderDemo();
    expect(await screen.findByTestId(`demo-card-${FIRST.id}`)).toBeInTheDocument();
  });

  it("renders the overview, the command walkthrough, the policy and the setup-it-yourself steps", async () => {
    renderDemo();
    expect(await screen.findByText(FIRST.overview)).toBeInTheDocument();
    expect(screen.getByTestId("demo-steps")).toBeInTheDocument();
    expect(screen.getByTestId(`demo-policy-${FIRST.id}`)).toBeInTheDocument();
    expect(screen.getByText(/set up a sandbox like this yourself/i)).toBeInTheDocument();
  });

  it("Start posts { agent, interactive, inline_policy } with the demo's exact policy", async () => {
    renderDemo();
    await user.click(await screen.findByTestId(`demo-start-${FIRST.id}`));
    expect(createRunMock).toHaveBeenCalledWith({
      agent: "claude-code",
      interactive: true,
      inline_policy: FIRST.policy,
      task_mode: "exec",
    });
  });

  it("Start on the harness demo posts an interactive run with NO task_mode=exec (it needs the model)", async () => {
    const harness = DEMOS.find((d) => d.needsModel)!;
    renderDemo(harness);
    await user.click(await screen.findByTestId(`demo-start-${harness.id}`));
    expect(createRunMock).toHaveBeenCalledWith({
      agent: "claude-code",
      interactive: true,
      inline_policy: harness.policy,
      task_mode: undefined,
    });
  });

  it("gates Start on barrierReady — disabled + the finish-Environment hint", async () => {
    renderDemo(FIRST, false);
    expect(await screen.findByTestId("demos-step-not-ready")).toBeInTheDocument();
    expect(screen.getByTestId(`demo-start-${FIRST.id}`)).toBeDisabled();
  });

  it("an active demo shows the terminal, live approvals, the inline audit panel, and End demo", async () => {
    renderDemo();
    await user.click(await screen.findByTestId(`demo-start-${FIRST.id}`));
    expect(await screen.findByTestId("attach-terminal")).toHaveTextContent("demo-run-1");
    expect(screen.getByTestId("live-approvals")).toBeInTheDocument();
    expect(screen.getByTestId("demo-audit-panel")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /end demo/i })).toBeInTheDocument();
  });

  // The audit panel is section-aware (phase 1's widened projection): the secrets
  // demos' credential events don't fit the egress-only shape, so the header says
  // "decisions", not "egress decisions".
  it("a secrets-section demo's audit panel drops the egress-only framing", async () => {
    const secretsDemo = DEMOS.find((d) => d.section === "secrets")!;
    renderDemo(secretsDemo);
    await user.click(await screen.findByTestId(`demo-start-${secretsDemo.id}`));
    const panel = await screen.findByTestId("demo-audit-panel");
    expect(panel).toHaveTextContent("Audit — decisions");
    expect(panel).not.toHaveTextContent("egress decisions");
  });

  // W3-S1-5: a kill that LOSES the server's "state changed concurrently" race
  // does not tear the sandbox down — the run is still live server-side. End
  // demo must not treat that 409 like a clean stop: it must surface the
  // failure and keep tracking the run (UI + localStorage), or the operator is
  // told "ended" while a sandbox keeps running unattended.
  it("a losing-race 409 on End demo keeps the run tracked and toasts an error, instead of orphaning it", async () => {
    renderDemo();
    await user.click(await screen.findByTestId(`demo-start-${FIRST.id}`));
    await screen.findByTestId("attach-terminal");
    expect(JSON.parse(localStorage.getItem("wardyn-demo-runs")!)).toEqual({ [FIRST.id]: "demo-run-1" });

    killRunMock.mockRejectedValueOnce(
      new HttpError(409, "run state changed concurrently; not overwriting with KILLED"),
    );
    await user.click(screen.getByRole("button", { name: /end demo/i }));

    expect(toastErrorMock).toHaveBeenCalledTimes(1);
    // Still tracked: the terminal stays mounted and localStorage still holds
    // the run, so a reload re-attaches instead of losing the only handle to it.
    expect(screen.getByTestId("attach-terminal")).toBeInTheDocument();
    expect(JSON.parse(localStorage.getItem("wardyn-demo-runs")!)).toEqual({ [FIRST.id]: "demo-run-1" });
  });

  // The OTHER 409 shape ("already terminal") means the run genuinely ended on
  // its own — that race is benign and End demo must still clean up quietly (no
  // toast, never re-attach on reload). The run stays tracked in memory rather
  // than being force-forgotten: the next poll tick confirms the real terminal
  // state and the step settles into its terminated view ("Turn this into a
  // policy" / Start again) instead of the live terminal.
  it("an already-terminal 409 on End demo forgets it quietly (no toast); the poll settles into the terminated view", async () => {
    renderDemo();
    await user.click(await screen.findByTestId(`demo-start-${FIRST.id}`));
    await screen.findByTestId("attach-terminal");

    killRunMock.mockRejectedValueOnce(new HttpError(409, "run is already terminal (state=KILLED); not re-killing"));
    getRunMock.mockResolvedValue({ id: "demo-run-1", state: "KILLED" });
    await user.click(screen.getByRole("button", { name: /end demo/i }));

    expect(toastErrorMock).not.toHaveBeenCalled();
    expect(localStorage.getItem("wardyn-demo-runs")).toBeNull();
    // The poll ticks every 2s (usePoll's real setInterval) — give it room to fire.
    await waitFor(() => expect(screen.queryByTestId("attach-terminal")).not.toBeInTheDocument(), {
      timeout: 3000,
    });
    expect(await screen.findByTestId("demo-terminated")).toBeInTheDocument();
  });

  // H-2: the "Turn this into a policy" payoff was wired ONLY in the deleted
  // DemoScreen (profileRunId state + ProfileReview mount + the onTurnIntoPolicy
  // prop DemoRunControls needs to render the button at all), while
  // record-a-policy's own steps still tell the operator to click it. It moved
  // here with the renderer, testid and all.
  it("a terminated run offers 'Turn this into a policy', and clicking it opens ProfileReview on that run", async () => {
    renderDemo();
    await user.click(await screen.findByTestId(`demo-start-${FIRST.id}`));
    await screen.findByTestId("attach-terminal");

    getRunMock.mockResolvedValue({ id: "demo-run-1", state: "COMPLETED" });
    const turnInto = await screen.findByTestId(`demo-turn-into-policy-${FIRST.id}`, undefined, {
      timeout: 3000,
    });
    expect(screen.queryByTestId("profile-review")).toBeNull();
    await user.click(turnInto);
    expect(await screen.findByTestId("profile-review")).toHaveTextContent("demo-run-1");
  });

  // authorized-not-issued's mint command carries a literal "{grant_id}" token —
  // StepList must render it AS-IS before a run exists (the "what you'll run"
  // preview has no run to ask), then substitute the real id once one is live.
  it("StepList renders {grant_id} literally pre-launch, and substitutes it once a run's grant is fetched", async () => {
    getGrantsMock.mockResolvedValue([{ id: "grant-123", scope: "api_key", audience: "api_key", state: "active" }]);
    const demo = DEMOS.find((d) => d.id === "authorized-not-issued")!;
    renderDemo(demo);

    expect((await screen.findAllByText(/\{grant_id\}/)).length).toBeGreaterThan(0);

    await user.click(screen.getByTestId(`demo-start-${demo.id}`));
    await waitFor(() => expect(screen.getAllByText(/grant-123/).length).toBeGreaterThan(0));
    expect(screen.queryAllByText(/\{grant_id\}/)).toHaveLength(0);
  });

  // ── the TEACH+GATE lane (needsGitHubApp) ───────────────────────────────────
  // Deliberately NOT the needsSecret/needsModel shape: those demos are dropped
  // from the walk entirely, which is fine for a card that is meaningless
  // without its prerequisite. This one's whole job is to teach a lane nothing
  // local can fake, so it keeps its place and closes its Start instead.
  it("a needsGitHubApp demo renders its gate and a DISABLED Start when no App is configured", async () => {
    renderDemo(APP_GATED, true, { githubAppReady: false });
    expect(await screen.findByTestId("demo-needs-github-app")).toBeInTheDocument();
    expect(screen.getByTestId(`demo-start-${APP_GATED.id}`)).toBeDisabled();
    // The card itself is intact — a gate must never cost the teaching.
    expect(screen.getByTestId("demo-steps")).toBeInTheDocument();
    expect(screen.getByTestId(`demo-policy-${APP_GATED.id}`)).toBeInTheDocument();
  });

  it("the same demo opens normally once an App IS configured", async () => {
    renderDemo(APP_GATED, true, { githubAppReady: true });
    await screen.findByTestId(`demo-card-${APP_GATED.id}`);
    expect(screen.queryByTestId("demo-needs-github-app")).toBeNull();
    expect(screen.getByTestId(`demo-start-${APP_GATED.id}`)).toBeEnabled();
  });

  // Member redaction zeroes SetupStatus.Secrets, so github_app reads false for
  // a member whether or not an App exists — "go add one under Settings" would
  // be false twice over (they cannot write secrets either).
  it("the gate copy tells a MEMBER to ask an operator, and an OPERATOR where to go", async () => {
    renderDemo(APP_GATED, true, { githubAppReady: false, operator: false });
    expect(await screen.findByTestId("demo-needs-github-app")).toHaveTextContent(/ask an operator/i);
    expect(screen.queryByRole("link", { name: /settings/i })).toBeNull();

    cleanup();
    renderDemo(APP_GATED, true, { githubAppReady: false, operator: true });
    const gate = await screen.findByTestId("demo-needs-github-app");
    expect(gate).not.toHaveTextContent(/ask an operator/i);
    expect(within(gate).getByRole("link", { name: /settings/i })).toHaveAttribute("href", "/settings");
  });

  // ── a REFUSED run-create ───────────────────────────────────────────────────
  // sts-fail-closed's lesson IS the 422: the mint is unreachable, so the run
  // never starts. A toast would scroll away mid-take; the card is where the
  // operator (and the e2e) is already looking.
  it("a 422 at create renders on the card, not in a toast — and still earns the demo its checkmark", async () => {
    const onDemoLaunched = vi.fn();
    createRunMock.mockRejectedValueOnce(
      new HttpError(422, "policy requires the spire identity provider: embedded identity: cloud_sts grant requires the spire identity provider"),
    );
    renderDemo(REFUSED_AT_CREATE, true, { onDemoLaunched });

    await user.click(await screen.findByTestId(`demo-start-${REFUSED_AT_CREATE.id}`));
    const refused = await screen.findByTestId("demo-create-refused");
    expect(refused).toHaveTextContent(/spire identity provider/i);
    expect(toastErrorMock).not.toHaveBeenCalled();
    // Nothing started, so nothing is tracked for a reload to re-attach.
    expect(localStorage.getItem("wardyn-demo-runs")).toBeNull();
    // …but the operator SAW what the card teaches, so it counts as done.
    expect(onDemoLaunched).toHaveBeenCalledWith(REFUSED_AT_CREATE.id);
    expect(JSON.parse(localStorage.getItem("wardyn-demos-launched")!)).toEqual([
      REFUSED_AT_CREATE.id,
    ]);
  });

  it("a NON-422 create failure also lands on the card, but never counts as demonstrated", async () => {
    const onDemoLaunched = vi.fn();
    createRunMock.mockRejectedValueOnce(new HttpError(403, "operator role required"));
    renderDemo(FIRST, true, { onDemoLaunched });

    await user.click(await screen.findByTestId(`demo-start-${FIRST.id}`));
    expect(await screen.findByTestId("demo-create-refused")).toHaveTextContent(/operator role required/i);
    expect(onDemoLaunched).not.toHaveBeenCalled();
    expect(localStorage.getItem("wardyn-demos-launched")).toBeNull();
  });

  it("starting again clears a previous refusal instead of stacking two verdicts", async () => {
    createRunMock.mockRejectedValueOnce(new HttpError(422, "runner \"none\" cannot enforce confinement_class CC3"));
    renderDemo(REFUSED_AT_CREATE);

    await user.click(await screen.findByTestId(`demo-start-${REFUSED_AT_CREATE.id}`));
    await screen.findByTestId("demo-create-refused");

    await user.click(screen.getByTestId(`demo-start-${REFUSED_AT_CREATE.id}`));
    await waitFor(() => expect(screen.queryByTestId("demo-create-refused")).not.toBeInTheDocument());
  });
});
