/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// MCP servers that run in the sandbox (features.mcp_local_servers): the Add dialog's two tabs, the
// door card, the added row, the credential warning, harness setup and the Review lines.

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { setField } from "../../../../../../test/set-field";
import { MY_COMPONENTS, SAVED_SECRETS } from "../../../../../lib/new-run-fixtures/scenarios/catalogue";
import { SCENARIO_PREFLIGHT } from "../../../../../lib/new-run-fixtures/scenarios/preflight";
import { SCENARIO_ACCESS } from "../../../../../lib/new-run-fixtures/scenarios/tools";
import { CLAUDE_UNGATED, CODEX, MCP_LOCAL, MCP_LOCAL_ENV, MODEL_PROVIDERS, PROVIDER_ACCESS, withFacts } from "../../../../../lib/new-run-fixtures/scenarios/tools-harness";
import type { AccessFacts } from "../../../../../lib/types/new-run-access";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { initialDraft, type RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { EffectivePolicyCard } from "../../review/effective-policy-card";
import { AccessGroup } from "../../review/groups/access-group";
import { AnnounceContext } from "../step-shell";
import { AddTools } from "./catalogue";
import { mcpLocalRef } from "./catalogue-edits";
import { HarnessSetup } from "./harness-setup";
import { Totals } from "./totals";

const h = vi.hoisted(() => ({ features: undefined as { mcp_local_servers?: boolean } | undefined, mine: vi.fn(), names: vi.fn() }));
vi.mock("../../../../../lib/api/components", () => ({ components: { mine: h.mine, saveMine: vi.fn() } }));
vi.mock("../../../../../lib/api/secrets", () => ({ secrets: { listSecrets: h.names } }));
vi.mock("../../../../wardyn/model-access-context", () => ({
  useModelAccessDoor: () => ({ openDoor: vi.fn(), open: false, operator: false }),
  useShellSetupStatus: () => ({
    status: {
      features: h.features, model_providers: MODEL_PROVIDERS, provider_access: PROVIDER_ACCESS,
      harnesses: [{ id: "claude-code", display: "Claude Code" }, { id: "codex-cli", display: "Codex CLI" }],
    },
    refresh: () => {},
  }),
}));

const NAMES = new Map([["claude-code", "Claude Code"], ["codex-cli", "Codex CLI"]]);
const DOCS = mcpLocalRef("docs-search", "npx -y @example/mcp-server@1.4.2", ["docs.example.com"], { secret_name: "DOCS_TOKEN", delivery: { mode: "env", var: "DOCS_TOKEN" } });
const WITH_DOCS: RunIntent = { components: [DOCS], tools: [{ id: "claude-code", kind: "harness" }, { id: "codex-cli", kind: "harness" }] };
const ACCESS = { ...withFacts(SCENARIO_ACCESS.member, [CLAUDE_UNGATED, CODEX], [MCP_LOCAL]), credentials: [MCP_LOCAL_ENV] };
const on = () => { h.features = { mcp_local_servers: true }; };

function Seed({ intent, children }: { intent: RunIntent; children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const [seeded, setSeeded] = React.useState(false);
  React.useEffect(() => { dispatch({ type: "seed", origin: { kind: "import" }, intent }); setSeeded(true); }, [dispatch, intent]);
  return seeded ? <>{children}</> : null;
}
function Draft() {
  const { intent } = useNewRunDraft();
  return <pre data-testid="draft">{JSON.stringify({ components: intent.components, tools: intent.tools })}</pre>;
}
const draft = () => JSON.parse(screen.getByTestId("draft").textContent ?? "{}") as { components?: Record<string, unknown>[]; tools?: Record<string, unknown>[] };

beforeEach(() => {
  h.features = undefined;
  h.mine.mockResolvedValue({ ...MY_COMPONENTS, may_define: true, resident_delivery_allowed: true });
  h.names.mockResolvedValue(SAVED_SECRETS);
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

function Catalogue({ access = SCENARIO_ACCESS.member, intent = {} }: { access?: AccessFacts; intent?: RunIntent }) {
  const { intent: live } = useNewRunDraft();
  return <AddTools access={access} answered intent={live} answeredFor={intent.components} />;
}
async function openDialog(intent: RunIntent = {}, access?: AccessFacts) {
  render(
    <AnnounceContext.Provider value={vi.fn()}>
      <NewRunDraftProvider owner="alice"><Seed intent={intent}><Catalogue access={access} intent={intent} /><Draft /></Seed></NewRunDraftProvider>
    </AnnounceContext.Provider>,
  );
  await screen.findByRole("heading", { name: NR_TOOLS.ADD_TITLE });
  await waitFor(() => expect(h.mine).toHaveBeenCalled());
}
async function openMcp() {
  await openDialog();
  await userEvent.click(await screen.findByRole("button", { name: NR_TOOLS.MCP_CARD_BUTTON }));
  return within(await screen.findByRole("dialog", { name: NR_TOOLS.MCP_CARD_TITLE }));
}
const doorBody = () => (screen.getByRole("heading", { name: NR_TOOLS.MCP_CARD_TITLE }).closest("li") as HTMLElement).textContent;

describe("Add an MCP server", () => {
  it("is today's dialog with the switch off: no tabs, and the local line says Coming soon", async () => {
    await openDialog();
    expect(doorBody()).toContain(NR_TOOLS.MCP_CARD_BODY);
    expect(doorBody()).not.toContain(NR_TOOLS.MCP_CARD_BODY_LOCAL);
    await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.MCP_CARD_BUTTON }));
    const dialog = within(await screen.findByRole("dialog", { name: NR_TOOLS.MCP_CARD_TITLE }));
    expect(dialog.getByText(NR_TOOLS.MCP_LOCAL_SOON)).toBeTruthy();
    expect(dialog.queryByRole("button", { name: NR_TOOLS.MCP_TAB_LOCAL })).toBeNull();
    expect(dialog.queryByText(NR_TOOLS.DETAIL_WHERE)).toBeNull();
  });

  it("with the switch on, the door card and the dialog offer a server that runs in a sandbox", async () => {
    on();
    await openDialog();
    expect(doorBody()).toContain(NR_TOOLS.MCP_CARD_BODY_LOCAL);
    await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.MCP_CARD_BUTTON }));
    const dialog = within(await screen.findByRole("dialog", { name: NR_TOOLS.MCP_CARD_TITLE }));
    expect(dialog.getByRole("button", { name: NR_TOOLS.MCP_REMOTE })).toBeTruthy();
    expect(dialog.getByText(NR_TOOLS.MCP_INTRO)).toBeTruthy();
    expect(dialog.queryByText(NR_TOOLS.MCP_LOCAL_SOON)).toBeNull();
  });

  it("the local tab says where it runs: this run's sandbox, and its own sandbox as Coming soon", async () => {
    on();
    const dialog = await openMcp();
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.MCP_TAB_LOCAL }));
    const where = within(dialog.getByRole("group", { name: NR_TOOLS.DETAIL_WHERE }));
    const mine = where.getByRole("radio", { name: NR_TOOLS.MCP_WHERE_RUN }) as HTMLInputElement;
    expect(mine.checked).toBe(true);
    expect(mine).toHaveAccessibleDescription(NR_TOOLS.MCP_WHERE_RUN_HINT);
    const own = where.getByRole("radio", { name: new RegExp(NR_TOOLS.MCP_WHERE_OWN) }) as HTMLInputElement;
    expect(own.disabled).toBe(true);
    expect(own).toHaveAccessibleDescription(NR_TOOLS.MCP_WHERE_OWN_HINT);
    expect(where.getByText(NR_SHARED.COMING_SOON)).toBeTruthy();
    for (const text of [NR_TOOLS.MCP_COMMAND_HELP, NR_TOOLS.NOTE_COMMAND, NR_TOOLS.MCP_HOSTS_HELP, NR_TOOLS.MCP_DELIVERY_NOTE, NR_TOOLS.MCP_HARNESS_RULES]) {
      expect(dialog.getByText(text)).toBeTruthy();
    }
    expect(dialog.getByLabelText(NR_TOOLS.MCP_NAME)).toHaveAttribute("placeholder", NR_TOOLS.MCP_LOCAL_NAME_PLACEHOLDER);
    expect(dialog.getByLabelText(NR_TOOLS.CC_HOSTS)).toHaveAttribute("placeholder", NR_TOOLS.MCP_HOSTS_PLACEHOLDER);
  });

  it("refuses an empty command and an unpinned package where it is typed, and adds nothing", async () => {
    on();
    const dialog = await openMcp();
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.MCP_TAB_LOCAL }));
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(dialog.getByRole("alert")).toHaveTextContent(NR_TOOLS.MCP_ERR_COMMAND);
    setField(dialog.getByLabelText("Command"), "npx -y @example/mcp-server");
    expect(dialog.queryByRole("alert")).toBeNull();
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(dialog.getByRole("alert")).toHaveTextContent(NR_TOOLS.MCP_ERR_PIN);
    expect(dialog.getByLabelText("Command")).toHaveAccessibleDescription(new RegExp(NR_TOOLS.MCP_ERR_PIN));
    // What the server refuses and the pin check does not name: a relative program, and an npx flag.
    setField(dialog.getByLabelText("Command"), "uvx foo");
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(dialog.getByRole("alert")).toHaveTextContent(NR_TOOLS.MCP_ERR_PROGRAM("uvx"));
    setField(dialog.getByLabelText("Command"), "npx --package x");
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(dialog.getByRole("alert")).toHaveTextContent(NR_TOOLS.MCP_ERR_NPX_FLAG);
    expect(draft().components).toBeUndefined();
  });

  it("adds a server with a secret placed as an environment variable", async () => {
    on();
    const dialog = await openMcp();
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.MCP_TAB_LOCAL }));
    setField(dialog.getByLabelText(NR_TOOLS.MCP_NAME), "docs-search");
    setField(dialog.getByLabelText("Command"), "npx -y @example/mcp-server@1.4.2");
    setField(dialog.getByLabelText(NR_TOOLS.CC_HOSTS), "docs.example.com");
    await userEvent.selectOptions(dialog.getByLabelText(NR_TOOLS.SECRET_LABEL), SAVED_SECRETS[0]);
    await userEvent.click(dialog.getByRole("radio", { name: NR_TOOLS.DELIVERY_ENV_OPTION }));
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(dialog.getByRole("alert")).toHaveTextContent(NR_TOOLS.SECRET_ERR_VARIABLE);
    setField(dialog.getByLabelText(NR_TOOLS.SECRET_VARIABLE_LABEL), "DOCS_TOKEN");
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(draft().components).toEqual([
      {
        name: "docs-search",
        inline: {
          hosts: ["docs.example.com"],
          secrets: [{ secret_name: SAVED_SECRETS[0], delivery: { mode: "env", var: "DOCS_TOKEN" } }],
          mcp: { transport: "stdio", launch: { command: "npx", args: ["-y", "@example/mcp-server@1.4.2"] } },
        },
      },
    ]);
  });

  it("adds a server with no secret, named for its package, and a header secret brings its host", async () => {
    on();
    let dialog = await openMcp();
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.MCP_TAB_LOCAL }));
    setField(dialog.getByLabelText("Command"), "npx -y @example/mcp-server@1.4.2");
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(draft().components).toEqual([{ name: "mcp-server", inline: { hosts: [], mcp: { transport: "stdio", launch: { command: "npx", args: ["-y", "@example/mcp-server@1.4.2"] } } } }]);

    cleanup();
    dialog = await openMcp();
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.MCP_TAB_LOCAL }));
    setField(dialog.getByLabelText(NR_TOOLS.MCP_NAME), "docs-search");
    setField(dialog.getByLabelText("Command"), "npx -y @example/mcp-server@1.4.2");
    await userEvent.selectOptions(dialog.getByLabelText(NR_TOOLS.SECRET_LABEL), SAVED_SECRETS[0]);
    setField(dialog.getByLabelText(NR_TOOLS.SECRET_HOST_LABEL), "api.example.com");
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(draft().components?.[0]).toMatchObject({ inline: { hosts: ["api.example.com"], secrets: [{ delivery: { mode: "header", host: "api.example.com" } }] } });
  });

  it("shows where a file secret is placed, and shuts env and file when the organisation has", async () => {
    on();
    const dialog = await openMcp();
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.MCP_TAB_LOCAL }));
    await userEvent.click(dialog.getByRole("radio", { name: NR_TOOLS.DELIVERY_FILE_OPTION }));
    setField(dialog.getByLabelText(NR_TOOLS.SECRET_FILE_LABEL), "docs-token");
    expect(dialog.getByText(NR_TOOLS.SECRET_FILE_PATH("docs-token"))).toBeTruthy();
    cleanup();
    h.mine.mockResolvedValue({ ...MY_COMPONENTS, may_define: true, resident_delivery_allowed: false });
    const closed = await openMcp();
    await userEvent.click(closed.getByRole("button", { name: NR_TOOLS.MCP_TAB_LOCAL }));
    expect((closed.getByRole("radio", { name: NR_TOOLS.DELIVERY_ENV_OPTION }) as HTMLInputElement).disabled).toBe(true);
    expect((closed.getByRole("radio", { name: NR_TOOLS.DELIVERY_HEADER_OPTION }) as HTMLInputElement).disabled).toBe(false);
  });

  it("shows an added local server on the shared tool card: what it brings, where it runs, who can read its secret", async () => {
    on();
    await openDialog(WITH_DOCS, ACCESS);
    const card = within(screen.getByRole("heading", { name: "docs-search", level: 3 }).closest("li") as HTMLElement);
    expect(card.getByText(NR_TOOLS.SUB_RUN_ONLY)).toBeTruthy();
    const brings = within(card.getByLabelText(NR_TOOLS.BRINGS_FOR("docs-search")));
    expect(brings.getByText("docs.example.com")).toBeTruthy();
    expect(brings.getByText("DOCS_TOKEN")).toBeTruthy();
    expect(brings.getByText(NR_TOOLS.DELIVERY_ENV)).toBeTruthy();
    expect(brings.getByText(NR_TOOLS.DETAIL_OTHER).nextElementSibling).toHaveTextContent(`${NR_TOOLS.FROM_MCP} · ${NR_TOOLS.MCP_RUNS_HERE}`);
    await userEvent.click(card.getByRole("button", { name: NR_TOOLS.DETAILS_FOR("docs-search") }));
    const details = within(await screen.findByRole("dialog"));
    expect(details.getByText(NR_TOOLS.MCP_SANDBOX_READABLE)).toBeTruthy();
    expect(details.getByText("npx -y @example/mcp-server@1.4.2")).toBeTruthy();
    // The Details dialog repeats "This run only" under the name, and the harness rules sit under the list, not in its cell.
    expect(details.getByText(NR_TOOLS.SUB_RUN_ONLY)).toBeTruthy();
    expect(details.getByText(NR_TOOLS.DETAIL_MCP_BODY).tagName).toBe("DD");
    expect(details.getByText(NR_TOOLS.MCP_HARNESS_RULES).tagName).toBe("P");
    // A header the proxy adds never enters the sandbox: the card says nothing of the kind.
    cleanup();
    await openDialog(WITH_DOCS, { ...ACCESS, credentials: [{ ...MCP_LOCAL_ENV, delivery: "proxy" }] });
    await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.DETAILS_FOR("docs-search") }));
    expect(within(await screen.findByRole("dialog")).queryByText(NR_TOOLS.MCP_SANDBOX_READABLE)).toBeNull();
    // A server that is not a local one keeps today's added row.
    cleanup();
    await openDialog(WITH_DOCS, withFacts(SCENARIO_ACCESS.member, [], [{ ...MCP_LOCAL, mcp: { ...MCP_LOCAL.mcp!, transport: "http" } }]));
    expect(screen.queryByText(NR_TOOLS.MCP_RUNS_HERE)).toBeNull();
  });
});

describe("Credentials total", () => {
  const totals = (credentials = [MCP_LOCAL_ENV]) => render(<Totals access={{ ...ACCESS, credentials }} answered />);

  it("tells the person that an environment variable or a file of a local server is readable by the agent", () => {
    totals();
    expect(screen.getByText(NR_TOOLS.MCP_SANDBOX_READABLE)).toBeTruthy();
    cleanup();
    totals([{ ...MCP_LOCAL_ENV, delivery: "sandbox_file" }]);
    expect(screen.getByText(NR_TOOLS.MCP_SANDBOX_READABLE)).toBeTruthy();
  });

  it("says nothing of the kind for a header the proxy adds, which never enters the sandbox", () => {
    totals([{ ...MCP_LOCAL_ENV, delivery: "proxy", after_end: "gone" }]);
    expect(screen.queryByText(NR_TOOLS.MCP_SANDBOX_READABLE)).toBeNull();
    expect(screen.getByText(NR_TOOLS.DELIVERY_PROXY)).toBeTruthy();
  });
});

describe("Harness setup", () => {
  function Live() {
    const { intent } = useNewRunDraft();
    return <HarnessSetup access={ACCESS} intent={intent} answered names={NAMES} onSignedIn={vi.fn()} />;
  }
  const show = (intent: RunIntent = WITH_DOCS) =>
    render(<NewRunDraftProvider owner="alice"><Seed intent={intent}><Live /><Draft /></Seed></NewRunDraftProvider>);
  const card = (name: string) => within(screen.getByRole("region", { name }));
  const rule = (effect: "hold" | "deny"): RunIntent => ({ ...WITH_DOCS, tools: [{ id: "claude-code", kind: "harness" }, { id: "codex-cli", kind: "harness", tool_rules: [{ tool: "mcp__docs-search__write_page", effect }] }] });

  it("marks a local server and says its rules are the harness's", () => {
    on();
    show();
    const claude = card("Claude Code");
    expect(claude.getByText("Runs in this run's sandbox")).toBeTruthy();
    expect(claude.getByText(NR_TOOLS.MCP_HARNESS_RULES)).toBeTruthy();
    expect(claude.queryByRole("alert")).toBeNull();
    expect(card("Codex CLI").getByText(NR_TOOLS.MCP_HARNESS_RULES)).toBeTruthy();
  });

  it.each(["hold", "deny"] as const)("refuses the run where Codex CLI is asked to %s a tool it cannot rule on, until the server is off for it", async (effect) => {
    on();
    show(rule(effect));
    const codex = card("Codex CLI");
    expect(codex.getByRole("alert")).toHaveTextContent(NR_TOOLS.MCP_REFUSED("Codex CLI", "docs-search"));
    expect(codex.queryByText(NR_TOOLS.MCP_HARNESS_RULES)).toBeNull();
    expect(card("Claude Code").queryByRole("alert")).toBeNull();
    await userEvent.click(codex.getByRole("checkbox", { name: "docs-search" }));
    expect(codex.queryByRole("alert")).toBeNull();
    expect(codex.getByText(NR_TOOLS.MCP_HARNESS_RULES)).toBeTruthy();
  });

  it("says Codex CLI's remote MCP tools only while local servers are on", async () => {
    show();
    expect(screen.getByText(NR_TOOLS.CODEX_NOTE)).toBeTruthy();
    cleanup();
    on();
    show();
    expect(screen.getByText(NR_TOOLS.CODEX_NOTE_REMOTE)).toBeTruthy();
    expect(screen.queryByText(NR_TOOLS.CODEX_NOTE)).toBeNull();
    await userEvent.click(card("Codex CLI").getByRole("button", { name: NR_TOOLS.ADD_RULE }));
    setField(screen.getByLabelText(NR_TOOLS.RULE_TOOL), "WebFetch");
    expect(screen.getAllByText("Wardyn can't block Codex CLI's own tools. Rules for its remote MCP tools still apply.").length).toBeGreaterThan(0);
  });

  it("keeps the golden's sentence in the rule dialog with the switch off", async () => {
    show();
    await userEvent.click(card("Codex CLI").getByRole("button", { name: NR_TOOLS.ADD_RULE }));
    setField(screen.getByLabelText(NR_TOOLS.RULE_TOOL), "WebFetch");
    expect(screen.getAllByText("Wardyn can't block Codex CLI's own tools. Rules for its MCP tools still apply.").length).toBeGreaterThan(0);
    expect(screen.queryByText(/remote MCP tools/)).toBeNull();
  });
});

describe("Review", () => {
  const pre = { ...SCENARIO_PREFLIGHT.member, access: ACCESS };
  const draftOf = (intent: RunIntent) => ({ ...initialDraft("alice"), intent, base: intent });

  it("lists the secret in the sandbox and the server whose rules are the harness's in the policy card", () => {
    on();
    render(<EffectivePolicyCard draft={draftOf(WITH_DOCS)} pre={pre} preview={null} />);
    const credentials = within(document.querySelector("[data-policy-row=credentials]") as HTMLElement);
    expect(credentials.getByText(NR_REVIEW.POLICY_IN_SANDBOX("DOCS_TOKEN", "docs-search"))).toBeTruthy();
    const tools = within(document.querySelector("[data-policy-row=tools]") as HTMLElement);
    expect(tools.getByText(NR_REVIEW.POLICY_HARNESS_RULES("docs-search"))).toBeTruthy();
  });

  it("has neither line when no local server is on the run", () => {
    render(<EffectivePolicyCard draft={draftOf(WITH_DOCS)} pre={{ ...pre, access: SCENARIO_ACCESS.member }} preview={null} />);
    expect(screen.queryByText(/In the sandbox:/)).toBeNull();
    expect(screen.queryByText(/rules applied by the harness/)).toBeNull();
  });

  it("lists each credential under the hosts of the Access group, with how it is delivered, while the switch is on", async () => {
    const group = () => render(<AccessGroup draft={draftOf(WITH_DOCS)} pre={pre} preview={null} docRows={[]} go={() => {}} dispatch={() => {}} />);
    group();
    await userEvent.click(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}` }));
    expect(screen.queryByRole("heading", { name: NR_TOOLS.CREDENTIALS_TITLE })).toBeNull();
    cleanup();
    on();
    group();
    await userEvent.click(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}` }));
    expect(screen.getByRole("heading", { name: NR_TOOLS.CREDENTIALS_TITLE })).toBeTruthy();
    expect(screen.getByText("DOCS_TOKEN")).toBeTruthy();
    expect(screen.getByText(NR_TOOLS.FOR_ROW("docs-search"))).toBeTruthy();
    expect(screen.getByText(NR_TOOLS.DELIVERY_ENV)).toBeTruthy();
  });
});
