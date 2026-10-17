/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { adoCapName } from "../../../../../lib/ado-access-copy";
import { ADO_PAT } from "../../../../../lib/ado-pat-copy";
import { secrets } from "../../../../../lib/api/secrets";
import { SCENARIO_ACCESS, SETUP_ITEMS } from "../../../../../lib/new-run-fixtures/scenarios/tools";
import { CLAUDE_UNGATED, withFacts } from "../../../../../lib/new-run-fixtures/scenarios/tools-harness";
import type { AccessFacts } from "../../../../../lib/types/new-run-access";
import type { HarnessSetFact } from "../../../../../lib/types/new-run-contract";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft, type NewRunScreenProps } from "../../new-run-route";
import type { RunIntent } from "../../draft/run-intent";
import { checkFailureText } from "../../draft/use-intent-checks";
import { HttpError } from "../../../../../lib/api/core";
import { ToolsStep } from "./tools-step";

const h = vi.hoisted(() => ({
  params: vi.fn(),
  providers: [] as unknown[],
  checks: { preview: { result: undefined as unknown, current: false }, invalidateChecks: vi.fn() },
  scm: undefined as unknown,
  refresh: vi.fn(),
  connect: vi.fn(),
}));
vi.mock("../../draft/use-intent-checks", async (orig) => ({
  ...(await orig<typeof import("../../draft/use-intent-checks")>()),
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 1, authGeneration: 0 }),
  useIntentChecks: (params: unknown) => { h.params(params); return h.checks; },
}));
vi.mock("../../../../wardyn/model-access-context", () => ({
  useModelAccessDoor: () => ({ openDoor: vi.fn(), open: false, refresh: h.refresh }),
  useShellSetupStatus: () => ({
    status: { harnesses: [{ id: "claude-code", display: "Claude Code" }, { id: "codex-cli", display: "Codex CLI" }], model_providers: h.providers, scm_access: h.scm },
    refresh: () => {},
  }),
}));
// The catalogue's own read is its tests' (catalogue.test.tsx): here it stays unanswered.
vi.mock("../../../../../lib/api/components", () => ({ components: { mine: () => new Promise(() => {}), saveMine: vi.fn() } }));
vi.mock("../../../../../lib/hooks/use-ado-connect", () => ({
  useAdoConnect: () => ({ connecting: false, connect: h.connect, connectFallback: h.connect, cancel: () => {}, blockedUrl: null }),
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
  h.scm = undefined;
  h.refresh.mockReset();
  h.connect.mockReset().mockResolvedValue(true);
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
  const SENTENCE = "This tool cannot be added to this run.";

  it("an added inline component's row removes that component through the answer it came from", async () => {
    const B = { name: "Beta", inline: { hosts: ["b.example"] } };
    const C = { name: "Gamma", inline: { hosts: ["c.example"] } };
    const added = (id: string, name: string) => ({ id, kind: "custom" as const, from: "added" as const, name, status: "ready", hosts: [], credentials: [], source: { kind: "component" as const } });
    answer({ ...SCENARIO_ACCESS.member, rows: [...SCENARIO_ACCESS.member.rows, added("inline:0", "Beta"), added("inline:1", "Gamma")] }, true);
    const Components = () => <p data-testid="components">{JSON.stringify(useNewRunDraft().intent.components ?? null)}</p>;
    render(
      <NewRunDraftProvider owner="alice">
        <Seed intent={{ ...READY, components: [B, C] }}><ToolsStep {...props} /><Components /></Seed>
      </NewRunDraftProvider>,
    );
    await userEvent.click(await screen.findByRole("button", { name: `${NR_TOOLS.ROW_REMOVE} Gamma` }));
    expect(JSON.parse(screen.getByTestId("components").textContent!)).toEqual([B]);
  });
  it("while the next preview is out, the old second row removes its own component, not the one now in its place", async () => {
    const [A, B, C] = ["Alpha", "Beta", "Gamma"].map((name) => ({ name, inline: { hosts: [`${name.toLowerCase()}.example`] } }));
    const added = (name: string, n: number) => ({ id: `inline:${n}`, kind: "custom" as const, from: "added" as const, name, status: "ready", hosts: [], credentials: [], source: { kind: "component" as const } });
    answer({ ...SCENARIO_ACCESS.member, rows: [...SCENARIO_ACCESS.member.rows, ...[A, B, C].map((c, n) => added(c.name, n))] }, true);
    const Components = () => <p data-testid="components">{JSON.stringify(useNewRunDraft().intent.components ?? null)}</p>;
    render(
      <NewRunDraftProvider owner="alice">
        <Seed intent={{ ...READY, components: [A, B, C] }}><ToolsStep {...props} /><Components /></Seed>
      </NewRunDraftProvider>,
    );
    await screen.findByRole("button", { name: `${NR_TOOLS.ROW_REMOVE} Alpha` });
    // Every edit from here asks again, and no answer comes back: the rows on screen stay the first answer's.
    answer(undefined, false);
    await userEvent.click(screen.getByRole("button", { name: `${NR_TOOLS.ROW_REMOVE} Alpha` }));
    await userEvent.click(screen.getByRole("button", { name: `${NR_TOOLS.ROW_REMOVE} Beta` }));
    expect(JSON.parse(screen.getByTestId("components").textContent!)).toEqual([C]);
  });
  // A fresh element each time, so a rerender is not skipped as identical.
  const tree = () => (
    <NewRunDraftProvider owner="alice">
      <Seed intent={READY}><ToolsStep {...props} /><Revision /></Seed>
    </NewRunDraftProvider>
  );

  it("Check again stays mounted and focused while the re-check is out, with no all-clear and no cards", async () => {
    const retry = vi.fn().mockResolvedValue(undefined);
    h.checks.preview = { result: undefined, current: false, error: new Error(SENTENCE), busy: false, retry } as never;
    const view = render(tree());
    const button = screen.getByRole("button", { name: NR_REVIEW.CHECK_AGAIN });
    await userEvent.click(button);
    expect(retry).toHaveBeenCalledTimes(1);
    // The hook keeps the old error and turns busy on the moment the retry starts.
    h.checks.preview = { result: undefined, current: false, error: new Error(SENTENCE), busy: true, retry } as never;
    view.rerender(tree());
    expect(screen.getByRole("alert")).toHaveTextContent(SENTENCE);
    expect(screen.getByRole("button", { name: NR_REVIEW.CHECK_AGAIN })).toBe(button);
    expect(button).toHaveFocus();
    expect(button).toHaveAttribute("aria-busy", "true");
    await userEvent.click(button);
    expect(retry).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(NR_TOOLS.SETUP_ALL_SET)).toBeNull();
    expect(screen.queryByRole("heading", { name: NR_TOOLS.INCLUDED_TITLE })).toBeNull();
  });

  it("a refusal after an earlier answer shows no all-clear anywhere, footer included", () => {
    const retry = vi.fn();
    answer(SCENARIO_ACCESS.member, true);
    const view = render(tree());
    expect(screen.getByText(NR_TOOLS.STICKY_DONE)).toBeInTheDocument();
    h.checks.preview = { result: undefined, current: false, error: new Error(SENTENCE), busy: false, retry } as never;
    view.rerender(tree());
    expect(screen.getByRole("alert")).toHaveTextContent(SENTENCE);
    expect(screen.queryByText(NR_TOOLS.STICKY_DONE)).toBeNull();
    expect(screen.queryByText(NR_TOOLS.SETUP_ALL_SET)).toBeNull();
  });

  it("when Check again succeeds, focus moves to the step heading instead of dropping to the body", async () => {
    const retry = vi.fn().mockResolvedValue(undefined);
    h.checks.preview = { result: undefined, current: false, error: new Error(SENTENCE), busy: false, retry } as never;
    const view = render(tree());
    await userEvent.click(screen.getByRole("button", { name: NR_REVIEW.CHECK_AGAIN }));
    h.checks.preview = { result: undefined, current: false, error: new Error(SENTENCE), busy: true, retry } as never;
    view.rerender(tree());
    answer(SCENARIO_ACCESS.member, true);
    view.rerender(tree());
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("heading", { level: 1 })).toHaveFocus();
  });

  it("a failure that clears while focus is on another live control leaves focus there", () => {
    h.checks.preview = { result: undefined, current: false, error: new Error(SENTENCE), busy: false, retry: vi.fn() } as never;
    const view = render(tree());
    const elsewhere = screen.getByRole("button", { name: NR_SHARED.BACK });
    elsewhere.focus();
    answer(SCENARIO_ACCESS.member, true);
    view.rerender(tree());
    expect(screen.queryByRole("alert")).toBeNull();
    expect(elsewhere).toHaveFocus();
  });

  it("a failure with no sentence uses the shared fallback", () => {
    h.checks.preview = { result: undefined, current: false, error: new Error(""), busy: false, retry: vi.fn() } as never;
    render(tree());
    expect(screen.getByRole("alert")).toHaveTextContent(checkFailureText(new Error("")));
    expect(screen.getByRole("alert")).toHaveTextContent("No reason was given.");
  });

  it("before the first answer there is no \"Nothing else to set up\"", () => {
    answer(undefined, false);
    render(tree());
    expect(screen.queryByText(NR_TOOLS.SETUP_ALL_SET)).toBeNull();
  });

  it("a first preview answered 429 says it was not checked yet, as a status and not an alert", async () => {
    const retry = vi.fn().mockResolvedValue(undefined);
    h.checks.preview = { result: undefined, current: false, error: new HttpError(429, "slow down"), busy: false, retry } as never;
    show();
    expect(screen.getByText(NR_TOOLS.NOT_CHECKED)).toHaveAttribute("role", "status");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(NR_TOOLS.SETUP_ALL_SET)).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: NR_REVIEW.CHECK_AGAIN }));
    expect(retry).toHaveBeenCalledTimes(1);
  });

  it("a refused or failed preview shows the server's sentence and a way to ask again, never an all-clear", async () => {
    const retry = vi.fn().mockResolvedValue(undefined);
    const sentence = "No sandbox image on this server includes Claude Code and Codex CLI together, so the run was not created.";
    h.checks.preview = { result: undefined, current: false, error: new Error(sentence), busy: false, retry } as never;
    show();
    expect(screen.getByRole("alert")).toHaveTextContent(sentence);
    expect(screen.queryByText(NR_TOOLS.SETUP_ALL_SET)).toBeNull();
    expect(screen.queryByRole("heading", { name: NR_TOOLS.INCLUDED_TITLE })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: NR_REVIEW.CHECK_AGAIN }));
    expect(retry).toHaveBeenCalledTimes(1);
  });

  it("shows no harness row, tool-rules card or MCP card for a harness the run does not have", () => {
    const NAMELESS_ROW = { id: "agent:", kind: "harness", from: "harness", name: "", status: "ready", hosts: [], credentials: [], source: { kind: "harness", name: "" } } as unknown as AccessFacts["rows"][number];
    answer(withFacts(SCENARIO_ACCESS.app, [{ ...CLAUDE_UNGATED, harness: "" }], [NAMELESS_ROW]), true);
    show({ shape: "environment", experience: "interactive", no_repositories_or_drives: true });
    expect(screen.queryByText(NR_TOOLS.FROM_HARNESS)).toBeNull();
    expect(screen.queryByRole("heading", { name: NR_TOOLS.HARNESS_SETUP_TITLE })).not.toBeNull();
    expect(screen.queryByText(NR_TOOLS.MCP_FOR_HARNESS)).toBeNull();
    expect(document.body.textContent).not.toMatch(/for \./);
  });

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
    // The prototype's sticky bar: Back, the count, then Review run.
    const bar = screen.getByRole("contentinfo");
    expect(bar).toHaveClass("sticky");
    expect(bar.textContent?.startsWith(`${NR_SHARED.BACK}${NR_TOOLS.STICKY_ONE}${NR_TOOLS.STICKY_SPOKEN(NR_TOOLS.STICKY_ONE)}${NR_TOOLS.REVIEW_RUN}`)).toBe(true);
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

  it("holds the all-clear back while the next answer is out (FUG-L445)", async () => {
    answer(SCENARIO_ACCESS.member, true);
    const view = show();
    expect(await screen.findByText(NR_TOOLS.SETUP_ALL_SET)).toBeInTheDocument();
    answer(SCENARIO_ACCESS.member, false);
    view.rerender(<NewRunDraftProvider owner="alice"><Seed intent={READY}><ToolsStep {...props} /><Revision /></Seed></NewRunDraftProvider>);
    expect(screen.queryByText(NR_TOOLS.SETUP_ALL_SET)).not.toBeInTheDocument();
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

  const SPOKEN = NR_TOOLS.STICKY_SPOKEN;
  // The live region empties about a second after it speaks.
  const spokenGone = (words: string) => waitFor(() => expect(screen.queryByText(SPOKEN(words))).not.toBeInTheDocument(), { timeout: 3000 });

  it("announces the settled words once, then empties, and says nothing again when a recheck settles on the same words", async () => {
    answer(SCENARIO_ACCESS.app, true);
    const view = show();
    expect(await screen.findByText(NR_TOOLS.STICKY_ONE)).toBeInTheDocument();
    expect(screen.getByText(SPOKEN(NR_TOOLS.STICKY_ONE))).toBeInTheDocument();
    await spokenGone(NR_TOOLS.STICKY_ONE);
    await saveToken();
    await waitFor(() => expect(h.checks.invalidateChecks).toHaveBeenCalledTimes(1));
    expect(screen.queryByText(NR_TOOLS.STICKY_ONE)).not.toBeInTheDocument();
    answer({ ...SCENARIO_ACCESS.app, setup: [...SCENARIO_ACCESS.app.setup] }, true);
    again(view);
    expect(await screen.findByText(NR_TOOLS.STICKY_ONE)).toBeInTheDocument();
    expect(screen.queryByText(SPOKEN(NR_TOOLS.STICKY_ONE))).not.toBeInTheDocument();
  });

  it("leaves no all-clear to browse while the answer for a changed draft is out, and announces the new words when it arrives", async () => {
    answer(SCENARIO_ACCESS.member, true);
    const view = show();
    expect(await screen.findByText(NR_TOOLS.STICKY_DONE)).toBeInTheDocument();
    answer(SCENARIO_ACCESS.member, false);
    again(view);
    await waitFor(() => expect(screen.queryByText(NR_TOOLS.STICKY_DONE)).not.toBeInTheDocument());
    await spokenGone(NR_TOOLS.STICKY_DONE);
    answer(SCENARIO_ACCESS.app, true);
    again(view);
    expect(await screen.findByText(NR_TOOLS.STICKY_ONE)).toBeInTheDocument();
    expect(screen.getByText(SPOKEN(NR_TOOLS.STICKY_ONE))).toBeInTheDocument();
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

  it("a run carrying the Azure DevOps credential on a row that creates its token: the Credentials total says what it carries, and Connect re-reads the person and the run", async () => {
    const access: AccessFacts = {
      ...SCENARIO_ACCESS.member,
      rows: [...SCENARIO_ACCESS.member.rows, { id: "repo:ado", kind: "repository", from: "repository", name: "contoso", status: "needs_setup", hosts: [], credentials: ["repo:ado:credential"], source: { kind: "workspace" } }],
      credentials: [...SCENARIO_ACCESS.member.credentials, { id: "repo:ado:credential", row: "repo:ado", kind: "ado_signin", owner: "own", delivery: "proxy", after_end: "gone" }],
    };
    h.scm = { org: "https://dev.azure.com/contoso", kind: "azure_devops", token_mode: "minted_pat", state: "not_configured", default_profile: ["code_read"] };
    h.checks.preview = { result: { access, spec: { azure_devops_capabilities: ["code_read", "code_write"] } }, current: true };
    show();
    const line = await screen.findByTestId("ado-run-token-line");
    expect(line).toHaveTextContent(`${ADO_PAT.NEWRUN_LINE_PREFIX}${["code_read", "code_write"].map(adoCapName).join(", ")}${ADO_PAT.NEWRUN_LINE_SUFFIX}`);
    const note = screen.getByTestId("ado-launch-note");
    await userEvent.click(within(note).getByRole("button", { name: ADO_PAT.MEMBER_CONNECT }));
    expect(h.connect).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(h.refresh).toHaveBeenCalledTimes(1));
    expect(h.checks.invalidateChecks).toHaveBeenCalledTimes(1);
  });
});
