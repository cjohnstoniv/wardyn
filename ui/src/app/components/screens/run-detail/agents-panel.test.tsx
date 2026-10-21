/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F2MULTI-3: the Agents panel and the per-agent output filter on a run with two or more agents.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { RunDetailAgent, RunOutput, SetupStatus } from "../../../lib/types";
import { SCENARIO_HARNESSES } from "../../../lib/new-run-fixtures/scenarios/harnesses";
import { ModelAccessProvider } from "../../wardyn/model-access-context";
import { RUN_AGENTS } from "../../wardyn/copy";
import { agentLines, agentsSummary } from "./agents-panel";
import { OutputTab } from "./output-tab";

const getMock = vi.fn();
vi.mock("../../../lib/api/run-output", () => ({ runOutput: { get: (...a: unknown[]) => getMock(...a) } }));
vi.mock("../../../lib/api/health", () => ({ health: { health: () => Promise.resolve({}) } }));

const AGENTS: RunDetailAgent[] = [{ agent: "claude-code" }, { agent: "codex-cli", task: "Write tests that cover the login retry path." }];
const LINES = [
  "wardyn-agents: claude-code=running codex-cli=running",
  "[claude-code] Reading tests/login.spec.ts",
  "[codex-cli] Adding tests/login-retry.spec.ts",
  "[claude-code] Found a race.",
].join("\n");
const out = (o: Partial<RunOutput> = {}): RunOutput => ({
  output: LINES, truncated: false, complete: false, source: "stdout", incomplete: false, capture_gap: false, ...o,
});

// The server's roster names the harnesses ("Codex CLI"), as the shell hands it down.
const STATUS = { harnesses: SCENARIO_HARNESSES.full } as SetupStatus;

async function mount(live: boolean, o: Partial<RunOutput>) {
  getMock.mockResolvedValue(out(o));
  render(
    <ModelAccessProvider status={STATUS} onRefresh={() => {}}>
      <OutputTab runId="r1" live={live} state={live ? "RUNNING" : "FAILED"} agents={AGENTS} runAgent="claude-code" runTask="Fix the flaky login test in web-app." onGoRecording={() => {}} />
    </ModelAccessProvider>,
  );
  await act(async () => {});
}
const panel = () => screen.getByRole("region", { name: RUN_AGENTS.title });

beforeEach(() => getMock.mockReset());

describe("Agents panel", () => {
  it("running: each agent's harness, task and status, the summary and the exit rule", async () => {
    await mount(true, { agents: [{ agent: "claude-code", state: "running" }, { agent: "codex-cli", state: "exited", exit_code: 0 }] });
    const rows = within(panel()).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByText("Claude Code")).toBeInTheDocument();
    expect(within(rows[0]).getByText("Fix the flaky login test in web-app.")).toBeInTheDocument();
    expect(within(rows[0]).getByText("Running")).toBeInTheDocument();
    expect(within(rows[1]).getByText("Codex CLI")).toBeInTheDocument();
    expect(within(rows[1]).getByText("Completed")).toBeInTheDocument();
    expect(within(panel()).getByText("2 agents · 1 running, 1 finished")).toBeInTheDocument();
    expect(within(panel()).getByText(RUN_AGENTS.exitRule)).toBeInTheDocument();
  });

  it("ended with one failure: that agent's word and the failed summary", async () => {
    await mount(false, { complete: true, agents: [{ agent: "claude-code", state: "exited", exit_code: 0 }, { agent: "codex-cli", state: "exited", exit_code: 1 }] });
    expect(within(panel()).getByText("Failed")).toBeInTheDocument();
    expect(within(panel()).getByText("1 of 2 agents failed, so the run failed.")).toBeInTheDocument();
  });

  it("summaries: running, failed, all finished", () => {
    expect(agentsSummary(["RUNNING", "RUNNING"])).toBe("2 agents · 2 running, 0 finished");
    expect(agentsSummary(["COMPLETED", "COMPLETED"])).toBe("Both agents finished.");
    expect(agentsSummary(["COMPLETED", "COMPLETED", "COMPLETED"])).toBe("All 3 agents finished.");
    expect(agentsSummary(["COMPLETED", undefined])).toBeUndefined();
  });

  it("a run with one agent shows no panel and no filter", async () => {
    getMock.mockResolvedValue(out());
    render(<OutputTab runId="r1" live state="RUNNING" onGoRecording={() => {}} />);
    await act(async () => {});
    expect(screen.queryByRole("region", { name: RUN_AGENTS.title })).toBeNull();
    expect(screen.queryByRole("group", { name: RUN_AGENTS.filterLabel })).toBeNull();
  });
});

describe("Output filter", () => {
  it("All agents shows every line with its prefix; one agent shows only its lines, prefix kept, and Copy follows it", async () => {
    const user = userEvent.setup();
    await mount(true, {});
    const group = screen.getByRole("group", { name: RUN_AGENTS.filterLabel });
    expect(within(group).getAllByRole("button").map((b) => b.textContent)).toEqual([RUN_AGENTS.filterAll, "Claude Code", "Codex CLI"]);
    expect(screen.getByTestId("run-output-text").textContent).toBe(LINES);
    await user.click(within(group).getByRole("button", { name: "Codex CLI" }));
    expect(screen.getByTestId("run-output-text").textContent).toBe("[codex-cli] Adding tests/login-retry.spec.ts");
    expect(agentLines(LINES, "claude-code")).toBe("[claude-code] Reading tests/login.spec.ts\n[claude-code] Found a race.");
  });
});

describe("Agents panel: without the run's own list", () => {
  it("falls back to the agents the output names, with the run's task on the run's agent", async () => {
    getMock.mockResolvedValue(out({ agents: [{ agent: "claude-code", state: "running" }, { agent: "codex-cli", state: "running" }] }));
    render(
      <ModelAccessProvider status={STATUS} onRefresh={() => {}}>
        <OutputTab runId="r1" live state="RUNNING" runAgent="claude-code" runTask="Fix the flaky login test in web-app." onGoRecording={() => {}} />
      </ModelAccessProvider>,
    );
    await act(async () => {});
    expect(within(panel()).getAllByRole("listitem")).toHaveLength(2);
    expect(within(panel()).getByText("Fix the flaky login test in web-app.")).toBeInTheDocument();
    expect(within(panel()).getByText("2 agents · 2 running, 0 finished")).toBeInTheDocument();
  });
});
