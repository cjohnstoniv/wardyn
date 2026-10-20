/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F2MULTI-4: a pending tool call says which agent asked, only on a run with two or more agents.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { makeApproval } from "../../../test/factories";
import { SCENARIO_HARNESSES } from "../../lib/new-run-fixtures/scenarios/harnesses";
import type { RunDetail, SetupStatus } from "../../lib/types";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { PendingCard } from "./approvals-cards";

let mockAgents: { agent: string }[] | undefined;
vi.mock("../../lib/api/runs", () => ({
  runs: { getRun: async () => ({ id: "r1", agent: "claude-code", repo: "acme/web-app", title: "Login test fix", state: "RUNNING", confinement_class: "CC1", agents: mockAgents }) satisfies Partial<RunDetail> },
}));

const item = makeApproval({
  id: "a1", run_id: "r1", kind: "tool_call", state: "PENDING", requested_at: "",
  requested_scope: { tool: "Bash", cmd: "rm -rf node_modules && npm ci", harness: "codex-cli" },
});
const egress = makeApproval({
  id: "a2", run_id: "r1", kind: "egress_domain", state: "PENDING", requested_at: "",
  requested_scope: { host: "registry.npmjs.org", harness: "codex-cli" },
});
const show = (it = item) =>
  render(
    <MemoryRouter>
      <ModelAccessProvider status={{ harnesses: SCENARIO_HARNESSES.full } as SetupStatus} onRefresh={() => {}}>
        <PendingCard item={it} caps={null} onAct={() => {}} onAdoDecide={async () => {}} onPushDecide={async () => {}} />
      </ModelAccessProvider>
    </MemoryRouter>,
  );

describe("PendingCard: the asking agent", () => {
  it("names it, with the agent count, on a run with two agents", async () => {
    mockAgents = [{ agent: "claude-code" }, { agent: "codex-cli" }];
    show();
    const line = await screen.findByText((_, el) => el?.tagName === "P" && /^Asked by/.test(el.textContent ?? ""));
    expect(line).toHaveTextContent("Asked by Codex CLI, one of 2 agents in this run");
  });

  it("says nothing on a run with one agent", async () => {
    mockAgents = undefined;
    show();
    await screen.findByText("Login test fix");
    expect(screen.queryByText(/Asked by/)).toBeNull();
  });

  it("says nothing on an egress row, even on a run with two agents", async () => {
    mockAgents = [{ agent: "claude-code" }, { agent: "codex-cli" }];
    show(egress);
    await screen.findByText("Login test fix");
    expect(screen.queryByText(/Asked by/)).toBeNull();
  });
});
