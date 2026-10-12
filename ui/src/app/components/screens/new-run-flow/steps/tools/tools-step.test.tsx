/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { secrets } from "../../../../../lib/api/secrets";
import { SCENARIO_ACCESS, SETUP_ITEMS } from "../../../../../lib/new-run-fixtures/scenarios/tools";
import { CLAUDE_UNGATED, withFacts } from "../../../../../lib/new-run-fixtures/scenarios/tools-harness";
import type { AccessFacts } from "../../../../../lib/types/new-run-access";
import type { HarnessSetFact } from "../../../../../lib/types/new-run-contract";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft, type NewRunScreenProps } from "../../new-run-route";
import type { RunIntent } from "../../draft/run-intent";
import { ToolsStep } from "./tools-step";

const h = vi.hoisted(() => ({
  params: vi.fn(),
  providers: [] as unknown[],
  checks: { preview: { result: undefined as unknown, current: false }, invalidateChecks: vi.fn() },
}));
vi.mock("../../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 1, authGeneration: 0 }),
  useIntentChecks: (params: unknown) => { h.params(params); return h.checks; },
}));
vi.mock("../../../../wardyn/model-access-context", () => ({
  useModelAccessDoor: () => ({ openDoor: vi.fn(), open: false }),
  useShellSetupStatus: () => ({
    status: { harnesses: [{ id: "claude-code", display: "Claude Code" }, { id: "codex-cli", display: "Codex CLI" }], model_providers: h.providers },
    refresh: () => {},
  }),
}));

const READY: RunIntent = { shape: "environment", experience: "interactive", no_repositories_or_drives: true, tools: [{ id: "claude-code", kind: "harness" }] };
const go = vi.fn();
const answer = (access: AccessFacts | undefined, current: boolean, harness_set?: HarnessSetFact) => { h.checks.preview = { result: access ? { access, harness_set } : undefined, current }; };

function Seed({ intent, children }: { intent: RunIntent; children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const [seeded, setSeeded] = React.useState(false);
  React.useEffect(() => {
    dispatch({ type: "seed", origin: { kind: "import" }, intent });
    setSeeded(true);
  }, [dispatch, intent]);
  return seeded ? <>{children}</> : null;
}
function Revision() {
  return <p data-testid="revision">{useNewRunDraft().revision}</p>;
}
const props: NewRunScreenProps = { location: { screen: "step", step: "tools" }, go, startFromScratch: vi.fn() };
function show(intent: RunIntent = READY) {
  return render(
    <NewRunDraftProvider owner="alice">
      <Seed intent={intent}><ToolsStep {...props} /><Revision /></Seed>
    </NewRunDraftProvider>,
  );
}
const network = () => within(screen.getByRole("heading", { name: NR_TOOLS.NETWORK_TITLE }).parentElement as HTMLElement);

beforeEach(() => {
  h.providers = [];
  answer(undefined, false);
  vi.spyOn(secrets, "setSecret").mockResolvedValue(undefined);
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  h.params.mockReset();
  h.checks.invalidateChecks.mockReset();
  go.mockReset();
});

describe("Tools and access step", () => {
  it("frames step 5 of 5, with Review run as the one primary action and Back to Files", async () => {
    show();
    expect(await screen.findByRole("heading", { level: 1, name: NR_TOOLS.TITLE })).toHaveFocus();
    expect(screen.getByText(NR_SHARED.EYEBROW(5))).toBeInTheDocument();
    expect(screen.getByText(NR_TOOLS.LEDE)).toBeInTheDocument();
    expect(screen.getByRole("navigation", { name: NR_SHARED.PROGRESS_ARIA })).toHaveTextContent(NR_SHARED.STEP_TOOLS);
    await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.REVIEW_RUN }));
    expect(go).toHaveBeenCalledWith({ screen: "review", view: "pretty" });
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.BACK }));
    expect(go).toHaveBeenCalledWith({ screen: "step", step: "files" });
  });

  it("shows the provider the preview's harness_set says the server chose, and leaves the draft alone (R)", async () => {
    h.providers = [{ id: "only-one", name: "Only provider", kind: "api_key", harnesses: ["claude-code"], host: "h" }];
    answer(withFacts(SCENARIO_ACCESS.member, [CLAUDE_UNGATED]), true, { shared_model_access: false, harnesses: [{ id: "claude-code", starts: true, model_credential: { provider: "only-one", kind: "api_key", residency: "proxy" } }] });
    show();
    const select = (await screen.findByLabelText(NR_TOOLS.PROVIDER_LABEL)) as HTMLSelectElement;
    expect(select.value).toBe("only-one");
    expect(screen.queryByText(NR_TOOLS.PROVIDER_REQUIRED)).toBeNull();
  });

  it("goes to Review with setup unfinished: it becomes blockers there, not a stop here", async () => {
    answer(SCENARIO_ACCESS.app, true);
    show();
    await screen.findByText(NR_TOOLS.STICKY_ONE);
    expect(screen.getByRole("button", { name: NR_TOOLS.REVIEW_RUN })).toBeEnabled();
    await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.REVIEW_RUN }));
    expect(go).toHaveBeenCalledWith({ screen: "review", view: "pretty" });
  });

  it("asks only the policy preview, and sends no body while the draft has local issues", async () => {
    show({});
    await screen.findByRole("heading", { name: NR_TOOLS.TITLE });
    expect(h.params.mock.calls.at(-1)?.[0]).toMatchObject({ body: null, autoCheck: { local: false, backendArm: false, modelArm: false } });
    cleanup();
    h.params.mockReset();
    show();
    await screen.findByRole("heading", { name: NR_TOOLS.TITLE });
    expect(h.params.mock.calls.at(-1)?.[0].body).toMatchObject({ shape: "environment", mode_contract: "0.9" });
  });

  it("shows no totals, rows or setup count before the server has answered", async () => {
    show();
    await screen.findByRole("heading", { name: NR_TOOLS.TITLE });
    expect(screen.queryByText(NR_TOOLS.NO_HOSTS)).not.toBeInTheDocument();
    expect(screen.queryByText(NR_TOOLS.STICKY_DONE)).not.toBeInTheDocument();
    expect(screen.queryByText(NR_TOOLS.NET_ALLOWED)).not.toBeInTheDocument();
  });

  it("shows a host as Requested, never Allowed, until the server has answered this body", async () => {
    answer(SCENARIO_ACCESS.member, false);
    const view = show();
    await screen.findByRole("heading", { name: NR_TOOLS.TITLE });
    expect(network().getByText(NR_TOOLS.NET_REQUESTED)).toBeInTheDocument();
    expect(network().queryByText(NR_TOOLS.NET_ALLOWED)).not.toBeInTheDocument();
    expect(network().getByText(NR_TOOLS.NET_POLICY_CEILING, { exact: false })).toBeInTheDocument();

    answer(SCENARIO_ACCESS.member, true);
    view.rerender(<NewRunDraftProvider owner="alice"><Seed intent={READY}><ToolsStep {...props} /><Revision /></Seed></NewRunDraftProvider>);
    expect(await network().findByText(NR_TOOLS.NET_ALLOWED)).toBeInTheDocument();
    expect(network().getByText(NR_TOOLS.NET_POLICY_CHECKED, { exact: false })).toBeInTheDocument();
  });

  it("keeps the last answer on screen, back at Requested, while the next one is out", async () => {
    answer(SCENARIO_ACCESS.member, true);
    const view = show();
    expect(await network().findByText(NR_TOOLS.NET_ALLOWED)).toBeInTheDocument();
    answer(undefined, false);
    view.rerender(<NewRunDraftProvider owner="alice"><Seed intent={READY}><ToolsStep {...props} /><Revision /></Seed></NewRunDraftProvider>);
    expect(network().getByText(NR_TOOLS.NET_REQUESTED)).toBeInTheDocument();
    expect(network().queryByText(NR_TOOLS.NET_ALLOWED)).not.toBeInTheDocument();
  });

  it("names harnesses by their display name from the shell's setup status", async () => {
    answer(SCENARIO_ACCESS.interactive2, true);
    show();
    expect(await screen.findByText("Claude Code", { selector: "strong" })).toBeInTheDocument();
    expect(screen.getByText("Codex CLI", { selector: "strong" })).toBeInTheDocument();
    expect(screen.getByText(NR_TOOLS.SETUP_CHOOSE_PROVIDER("Codex CLI"))).toBeInTheDocument();
  });

  it("counts the items still needing setup, and says everything is set up when none are", async () => {
    answer({ ...SCENARIO_ACCESS.app, setup: [SCENARIO_ACCESS.app.setup[0], SCENARIO_ACCESS.interactive2.setup[0]] }, true);
    const view = show();
    expect(await screen.findByText(NR_TOOLS.STICKY_MANY(2))).toBeInTheDocument();
    answer(SCENARIO_ACCESS.member, true);
    view.rerender(<NewRunDraftProvider owner="alice"><Seed intent={READY}><ToolsStep {...props} /><Revision /></Seed></NewRunDraftProvider>);
    expect(await screen.findByText(NR_TOOLS.STICKY_DONE)).toBeInTheDocument();
    expect(screen.getByText(NR_TOOLS.SETUP_ALL_SET)).toBeInTheDocument();
  });

  const again = (view: ReturnType<typeof show>) =>
    view.rerender(<NewRunDraftProvider owner="alice"><Seed intent={READY}><ToolsStep {...props} /><Revision /></Seed></NewRunDraftProvider>);
  const saveToken = async () => {
    await userEvent.click(await screen.findByRole("button", { name: NR_TOOLS.SETUP_ADD }));
    await userEvent.type(screen.getByLabelText(NR_TOOLS.TOKEN_LABEL), "tok");
    await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.TOKEN_CONFIRM }));
    await waitFor(() => expect(secrets.setSecret).toHaveBeenCalledWith("deploy-key", "tok"));
  };

  it("counts only items the server reports missing: satisfied and unverified ones are neither listed nor counted", async () => {
    const base = SCENARIO_ACCESS.member;
    const unverified = { ...SETUP_ITEMS.addSecret, id: "git_provider:github:app", label: "GitHub App installation", status: "unverified" };
    answer({ ...base, setup: [unverified, { ...SETUP_ITEMS.addSecret, id: "secret:ok", label: "ok-key", status: "satisfied" }] }, true);
    show();
    expect(await screen.findByText(NR_TOOLS.STICKY_DONE)).toBeInTheDocument();
    expect(screen.getByText(NR_TOOLS.SETUP_ALL_SET)).toBeInTheDocument();
    expect(screen.queryByText("GitHub App installation")).not.toBeInTheDocument();
  });

  it("finishing a door shows neither Done nor Everything is set up until the server answers again", async () => {
    answer(SCENARIO_ACCESS.app, true);
    const view = show();
    expect(await screen.findByText(NR_TOOLS.STICKY_ONE)).toBeInTheDocument();
    await saveToken();
    await waitFor(() => expect(h.checks.invalidateChecks).toHaveBeenCalledTimes(1));
    expect(screen.queryByText(NR_TOOLS.SETUP_DONE)).not.toBeInTheDocument();
    expect(screen.queryByText(NR_TOOLS.STICKY_DONE)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: NR_TOOLS.SETUP_ADD })).not.toBeInTheDocument();

    answer({ ...SCENARIO_ACCESS.app, setup: [{ ...SETUP_ITEMS.addSecret, status: "satisfied" }] }, true);
    again(view);
    expect(await screen.findByText(NR_TOOLS.STICKY_DONE)).toBeInTheDocument();
    expect(screen.getByText(NR_TOOLS.SETUP_DONE)).toBeInTheDocument();
  });

  it("brings an item back with its door when the next answer still lists it as missing", async () => {
    answer(SCENARIO_ACCESS.app, true);
    const view = show();
    await saveToken();
    answer({ ...SCENARIO_ACCESS.app, setup: [...SCENARIO_ACCESS.app.setup] }, true);
    again(view);
    expect(await screen.findByText(NR_TOOLS.STICKY_ONE)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: NR_TOOLS.SETUP_ADD })).toBeInTheDocument();
  });

  it("holds the rows' Ready chips and the policy sentence while the answer is stale", async () => {
    answer(SCENARIO_ACCESS.member, false);
    show();
    await screen.findByRole("heading", { name: NR_TOOLS.TITLE });
    expect(screen.queryByText(NR_TOOLS.STATUS_READY)).not.toBeInTheDocument();
    expect(screen.queryByText(NR_TOOLS.NET_POLICY_DENY_REVIEW_YOU, { exact: false })).not.toBeInTheDocument();
  });

  it("adds nothing until the person chooses: opening rows and focusing doors leave the draft as it was", async () => {
    answer(SCENARIO_ACCESS.interactive2, true);
    show();
    const before = (await screen.findByTestId("revision")).textContent;
    for (const toggle of screen.getAllByRole("button", { name: /^Details for / })) await userEvent.click(toggle);
    await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.SETUP_CHOOSE }));
    expect(screen.getByTestId("revision").textContent).toBe(before);
    expect(screen.queryByRole("button", { name: NR_TOOLS.CARD_ADD })).not.toBeInTheDocument();
  });
});
