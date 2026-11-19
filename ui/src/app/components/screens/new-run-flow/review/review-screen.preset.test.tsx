/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Review and the chosen wall preset (boards WP-2): its line, the held Launch for a preset that is gone, and the
// effective policy card's Tools and Pushes rows.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { OperatorProvider } from "../../../wardyn/operator-context";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { WALL_PRESETS as W } from "../../../wardyn/copy/wall-presets";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import type { RunIntent } from "../draft/run-intent";
import { NEW_RUN_SCREENS, NewRunRoute, type Seed, type SeedLoaders } from "../new-run-route";
import { ReviewScreen } from "./review-screen";

const { checks, wfetchMock } = vi.hoisted(() => ({ checks: { current: {} as Record<string, unknown> }, wfetchMock: vi.fn() }));
vi.mock("../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 1, authGeneration: 0 }),
  useIntentChecks: () => checks.current,
}));
vi.mock("../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

const RUN_ID = "11111111-1111-4111-8111-111111111111";
const INERT = Object.fromEntries(Object.keys(NEW_RUN_SCREENS).map((name) => [name, () => null])) as unknown as typeof NEW_RUN_SCREENS;
const AGENT: RunIntent = {
  shape: "agent_task", experience: "interactive", runner_pool_id: "pool-general", no_repositories_or_drives: true,
  workload: { kind: "agent_task", agent: "claude-code" },
  tools: [{ id: "claude-code", kind: "harness" }],
  startup: { kind: "harness", tool: "claude-code", prompt: "Fix the build" },
};
const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });
const PRESETS = {
  presets: [
    { id: "open", scope: "builtin", name: "Open", spec: {}, version: 1 },
    { id: "sealed", scope: "builtin", name: "Sealed", spec: { push_rules: { deny_all: true } }, version: 1 },
    { id: "org-1", scope: "org", name: "Docs triage", spec: { allowed_domains: [] }, version: 4 },
  ],
  publish: { org: false, groups: [], groups_unverified: false },
};

function open(intent: RunIntent) {
  const loaders: SeedLoaders = {
    template: async (): Promise<Seed> => { throw new Error("no template"); },
    again: async (): Promise<Seed> => ({ origin: { kind: "again", runId: RUN_ID, title: "Nightly" }, intent }),
  };
  wfetchMock.mockImplementation(async (path: string) => {
    if (path.startsWith("/me/drives")) return json({ drives: [], pool_checked: true });
    if (path === "/me/walls") return json({ spec: {} });
    if (path === "/wall-presets") return json(PRESETS);
    if (path === "/templates") return json({ templates: [], publish: { org: false, groups: [], groups_unverified: false } });
    throw new Error(`unexpected ${path}`);
  });
  render(
    <MemoryRouter initialEntries={[`/runs/new?again=${RUN_ID}`]}>
      <OperatorProvider operator={false} principal="alice">
        <Routes>
          <Route path="/runs/new" element={<NewRunRoute loaders={loaders} screens={{ ...INERT, review: (p) => <ReviewScreen {...p} /> }} />} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}
const launch = () => screen.findByRole("button", { name: NR_REVIEW.LAUNCH_RUN });
const preview = (spec: Record<string, unknown>) => {
  (checks.current.preview as { result: unknown }).result = { spec, source: { kind: "default" } };
};

beforeEach(() => {
  checks.current = {
    generation: 5,
    preflightRead: { generation: 5, at: Date.now() - 1000 },
    preflight: vi.fn(),
    preflightResult: SCENARIO_PREFLIGHT.member,
    preflightError: null,
    preflightReason: "",
    preview: { result: null, retry: vi.fn() },
  };
});
afterEach(() => {
  cleanup();
  wfetchMock.mockReset();
  vi.restoreAllMocks();
});

describe("Review: the wall preset", () => {
  it("draws Wall preset: Sealed (built in) with a Change link, above the person's walls line", async () => {
    open({ ...AGENT, wall_preset: "sealed" });
    const line = await screen.findByTestId("wall-preset-line");
    expect(line).toHaveTextContent("Wall preset: Sealed (built in) Change");
    expect(within(line).getByRole("button", { name: W.CHANGE })).toBeInTheDocument();
  });

  it("names an organisation preset with its audience", async () => {
    open({ ...AGENT, wall_preset: "org-1" });
    expect(await screen.findByTestId("wall-preset-line")).toHaveTextContent("Wall preset: Docs triage (organisation)");
  });

  it("says nothing for Open, or when no preset was chosen", async () => {
    open({ ...AGENT, wall_preset: "open" });
    await launch();
    await waitFor(() => expect(wfetchMock.mock.calls.some(([p]) => p === "/wall-presets")).toBe(true));
    expect(screen.queryByTestId("wall-preset-line")).toBeNull();
  });

  it("holds Launch for a preset the list no longer has, keeps it chosen as an error, and shows no effective policy", async () => {
    open({ ...AGENT, wall_preset: "org-9" });
    expect(await screen.findByTestId("wall-preset-line")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeDisabled());
    expect(screen.getByText(W.UNAVAILABLE(W.UNAVAILABLE_UNNAMED))).toBeInTheDocument();
    expect(screen.getByText(W.PREVIEW_PENDING)).toBeInTheDocument();
    expect(screen.queryByLabelText(NR_REVIEW.POLICY_TITLE)).toBeNull();
    // Nothing fell back to Open: the line is still the chosen preset's.
    expect(screen.getByTestId("wall-preset-line")).not.toHaveTextContent("Open");
  });

  it("separates Tools and Pushes in the effective policy card, and a deny_all preview reads Every push refused", async () => {
    preview({ git_push_any_branch: false, push_rules: { deny_all: true } });
    open({ ...AGENT, wall_preset: "sealed" });
    const card = within(await screen.findByLabelText(NR_REVIEW.POLICY_TITLE));
    expect(card.getByText(W.ROW_TOOLS)).toBeInTheDocument();
    await waitFor(() => expect(document.querySelector("[data-policy-row=pushes]")).toHaveTextContent(W.PUSH_EVERY_REFUSED));
    expect(within(document.querySelector("[data-policy-row=pushes]") as HTMLElement).getByText(W.ROW_PUSHES)).toBeInTheDocument();
  });

  it.each([
    ["no write access on both forges", { git_push_any_branch: false, github_capabilities: ["code_read"], azure_devops_capabilities: ["code_read"] }, W.PUSH_NO_WRITE],
    ["no write access on one forge only", { git_push_any_branch: false, github_capabilities: ["code_read"] }, "GitHub: no pushes (no write access); Azure DevOps: only this run's own branch"],
    ["own branch", { git_push_any_branch: false }, "Only this run's own branch"],
    ["any branch", { git_push_any_branch: true }, "Any branch"],
  ])("Pushes reads for %s", async (_name, spec, text) => {
    preview(spec);
    open({ ...AGENT, wall_preset: "sealed" });
    await screen.findByLabelText(NR_REVIEW.POLICY_TITLE);
    await waitFor(() => expect(document.querySelector("[data-policy-row=pushes]")).toHaveTextContent(text));
  });

  it("Pushes reads Pushes can't be stopped on this install when the preview says so, not from the warning text", async () => {
    preview({ git_push_any_branch: false, github_capabilities: ["code_read"] });
    (checks.current.preview as { result: Record<string, unknown> }).result.pushes_unstoppable = true;
    open({ ...AGENT, wall_preset: "sealed" });
    await screen.findByLabelText(NR_REVIEW.POLICY_TITLE);
    await waitFor(() => expect(document.querySelector("[data-policy-row=pushes]")).toHaveTextContent(W.PUSHES_UNSTOPPABLE));
    expect(document.querySelector("[data-policy-row=pushes]")).not.toHaveTextContent(/no write access/);
  });

  // deny_all refuses an unbrokered push to a known forge host even with the brokers off, so it outranks the unstoppable line.
  it("never says Pushes can't be stopped under deny_all, and shows the secrets note when the preview carries it", async () => {
    preview({ git_push_any_branch: false, push_rules: { deny_all: true } });
    Object.assign((checks.current.preview as { result: Record<string, unknown> }).result, { pushes_unstoppable: true, push_deny_all_secrets: true });
    open({ ...AGENT, wall_preset: "sealed" });
    await screen.findByLabelText(NR_REVIEW.POLICY_TITLE);
    await waitFor(() => expect(document.querySelector("[data-policy-row=pushes]")).toHaveTextContent(W.PUSH_EVERY_REFUSED));
    const row = document.querySelector("[data-policy-row=pushes]");
    expect(row).not.toHaveTextContent(W.PUSHES_UNSTOPPABLE);
    expect(row).toHaveTextContent(W.PUSH_DENY_ALL_SECRETS);
  });

  it("shows no secrets note under deny_all when the preview does not carry the fact", async () => {
    preview({ git_push_any_branch: false, push_rules: { deny_all: true } });
    open({ ...AGENT, wall_preset: "sealed" });
    await screen.findByLabelText(NR_REVIEW.POLICY_TITLE);
    await waitFor(() => expect(document.querySelector("[data-policy-row=pushes]")).toHaveTextContent(W.PUSH_EVERY_REFUSED));
    expect(document.querySelector("[data-policy-row=pushes]")).not.toHaveTextContent(W.PUSH_DENY_ALL_SECRETS);
  });

  it("Change goes back to the Tools step", async () => {
    open({ ...AGENT, wall_preset: "sealed" });
    const line = await screen.findByTestId("wall-preset-line");
    await userEvent.setup().click(within(line).getByRole("button", { name: W.CHANGE }));
    // The Tools screen is inert here; the route moved off Review.
    await waitFor(() => expect(screen.queryByTestId("wall-preset-line")).toBeNull());
  });
});
