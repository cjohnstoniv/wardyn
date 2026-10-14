/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { setField } from "../../../../../../test/set-field";
import { SCENARIO_ACCESS } from "../../../../../lib/new-run-fixtures/scenarios/tools";
import {
  CLAUDE_ATTENDED, CLAUDE_GATED, CLAUDE_GATED_LOCKED, CLAUDE_UNGATED, CLAUDE_UNGATED_LOCKED, CODEX, CUSTOM, MCP_DOCS,
  MODEL_PROVIDERS, PROVIDER_ACCESS, withFacts,
} from "../../../../../lib/new-run-fixtures/scenarios/tools-harness";
import type { AccessFacts, ToolPolicyFact } from "../../../../../lib/types/new-run-access";
import type { HarnessSetFact } from "../../../../../lib/types/new-run-contract";
import type { SetupItem } from "../../../../../lib/types/preflight";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import type { RunIntent } from "../../draft/run-intent";
import { HarnessSetup } from "./harness-setup";
import { SetupNeeded } from "./setup-needed";
import { providerControlId } from "../../lenses/tools-lens";

const h = vi.hoisted(() => ({ openDoor: vi.fn(), operator: false, answered: true, providers: null as unknown[] | null }));
vi.mock("../../../../wardyn/model-access-context", () => ({
  useModelAccessDoor: () => ({ openDoor: h.openDoor, open: false, operator: h.operator }),
  useShellSetupStatus: () => ({ status: { model_providers: h.providers ?? MODEL_PROVIDERS, provider_access: PROVIDER_ACCESS }, refresh: () => {} }),
}));

const NAMES = new Map([["claude-code", "Claude Code"], ["codex-cli", "Codex CLI"], ["custom", "Your own tools"]]);
const BASE = SCENARIO_ACCESS.member;
const CLAUDE: RunIntent = { tools: [{ id: "claude-code", kind: "harness" }] };
const onSignedIn = vi.fn();

function Seed({ intent, children }: { intent: RunIntent; children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const [seeded, setSeeded] = React.useState(false);
  React.useEffect(() => { dispatch({ type: "seed", origin: { kind: "import" }, intent }); setSeeded(true); }, [dispatch, intent]);
  return seeded ? <>{children}</> : null;
}
/** The draft's tool entries and components, as the wire will carry them. */
function Draft() {
  const { intent } = useNewRunDraft();
  return <pre data-testid="draft">{JSON.stringify({ tools: intent.tools, components: intent.components })}</pre>;
}
function Live({ access, harnessSet }: { access: AccessFacts | undefined; harnessSet?: HarnessSetFact }) {
  const { intent } = useNewRunDraft();
  return <HarnessSetup access={access} harnessSet={harnessSet} intent={intent} answered={h.answered} names={NAMES} onSignedIn={onSignedIn} />;
}
/** The server asks for a choice (the preview's `choose_model_provider` item) only when it chose none. */
const CHOOSE: SetupItem = { id: "llm_access:claude-code", kind: "llm_access", label: "Choose a model provider", required_by: "claude-code", status: "missing", fix: { action: "choose_model_provider", tool: "claude-code" } };
/** The preview's harness_set when the server itself chose `provider` for Claude Code. */
const chose = (provider: string): HarnessSetFact => ({ shared_model_access: false, harnesses: [{ id: "claude-code", starts: true, model_credential: { provider, kind: "api_key", residency: "proxy" } }] });
function show(facts: ToolPolicyFact[] | undefined, intent: RunIntent = CLAUDE, rows = [MCP_DOCS], extra: { setup?: SetupItem[]; harnessSet?: HarnessSetFact } = {}) {
  return render(
    <NewRunDraftProvider owner="alice">
      <Seed intent={intent}><Live access={facts ? { ...withFacts(BASE, facts, rows), setup: extra.setup ?? [] } : undefined} harnessSet={extra.harnessSet} /><Draft /></Seed>
    </NewRunDraftProvider>,
  );
}
const draft = () => JSON.parse(screen.getByTestId("draft").textContent ?? "{}") as { tools: Record<string, unknown>[]; components: Record<string, unknown>[] };
const card = (name: string) => within(screen.getByRole("region", { name }));

beforeEach(() => { h.operator = false; h.answered = true; h.providers = null; });
afterEach(() => { cleanup(); vi.clearAllMocks(); });

describe("HarnessSetup", () => {
  it("shows nothing until the server has answered, and says so when the run has no harness", () => {
    const { container } = show(undefined);
    expect(container.querySelector("section")).toBeNull();
    cleanup();
    show([]);
    expect(screen.getByRole("heading", { name: NR_TOOLS.HARNESS_SETUP_TITLE })).toBeTruthy();
    expect(screen.getByText(NR_TOOLS.NO_HARNESSES)).toBeTruthy();
  });

  it("heads each harness with whether it starts with the run", () => {
    show([CLAUDE_UNGATED, CODEX], { tools: [{ id: "claude-code", kind: "harness" }, { id: "codex-cli", kind: "harness" }] });
    expect(card("Claude Code").getByText(NR_TOOLS.SUB_STARTS)).toBeTruthy();
    expect(card("Codex CLI").getByText(NR_TOOLS.SUB_AVAILABLE)).toBeTruthy();
  });

  describe("model provider", () => {
    it("lists the providers that serve the harness by record name, and never a switched-off one", () => {
      show([CLAUDE_UNGATED], CLAUDE, [MCP_DOCS], { setup: [CHOOSE] });
      const select = screen.getByLabelText(NR_TOOLS.PROVIDER_LABEL);
      expect(select.id).toBe(providerControlId("claude-code"));
      expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual([NR_TOOLS.PROVIDER_CHOOSE, "Anthropic API key", "Bedrock (SSO)"]);
      expect(screen.getByText(NR_TOOLS.PROVIDER_REQUIRED)).toBeTruthy();
    });

    it("writes the choice to the harness's own tool entry", async () => {
      show([CLAUDE_UNGATED], CLAUDE, [MCP_DOCS], { setup: [CHOOSE] });
      await userEvent.selectOptions(screen.getByLabelText(NR_TOOLS.PROVIDER_LABEL), "anthropic-key");
      expect(draft().tools).toEqual([{ id: "claude-code", kind: "harness", model_provider: "anthropic-key" }]);
      expect(screen.queryByText(NR_TOOLS.PROVIDER_REQUIRED)).toBeNull();
    });

    it("offers sign-in only when the person's credential for the chosen provider is missing", async () => {
      show([CLAUDE_UNGATED], { tools: [{ id: "claude-code", kind: "harness", model_provider: "anthropic-key" }] });
      expect(screen.queryByRole("button", { name: /Sign in/ })).toBeNull();
      await userEvent.selectOptions(screen.getByLabelText(NR_TOOLS.PROVIDER_LABEL), "bedrock-sso");
      await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.SETUP_SIGN_IN_TO("Bedrock (SSO)") }));
      expect(h.openDoor).toHaveBeenCalledWith(expect.objectContaining({ for: { provider: "bedrock-sso" } }));
    });

    it("is the control Setup still needed's Choose focuses", async () => {
      const item = { id: "llm_access:claude-code", kind: "llm_access", label: "Choose a model provider", required_by: "claude-code", status: "missing", fix: { action: "choose_model_provider", tool: "claude-code" } } as const;
      render(
        <NewRunDraftProvider owner="alice">
          <Seed intent={CLAUDE}>
            <Live access={withFacts(BASE, [CLAUDE_UNGATED])} />
            <SetupNeeded items={[item]} pending={new Set()} done={new Set()} intent={CLAUDE} names={NAMES} onDone={() => {}} />
          </Seed>
        </NewRunDraftProvider>,
      );
      await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.SETUP_CHOOSE }));
      expect(document.activeElement).toBe(screen.getByLabelText(NR_TOOLS.PROVIDER_LABEL));
      expect(document.activeElement?.id).toBe("nr-provider-claude-code");
    });

    it("still renders the provider control, off and required, when no provider serves the harness", () => {
      h.providers = [];
      show([CLAUDE_UNGATED], CLAUDE, [MCP_DOCS], { setup: [CHOOSE] });
      const select = screen.getByLabelText(NR_TOOLS.PROVIDER_LABEL) as HTMLSelectElement;
      expect(select.id).toBe("nr-provider-claude-code");
      expect(select.disabled).toBe(true);
      expect(screen.getByText(NR_TOOLS.PROVIDER_REQUIRED)).toBeTruthy();
    });

    it("shows the provider the server chose, with its delivery line, and neither asks for a choice nor writes one to the draft (R)", () => {
      show([CLAUDE_UNGATED], CLAUDE, [MCP_DOCS], { harnessSet: chose("anthropic-key") });
      expect((screen.getByLabelText(NR_TOOLS.PROVIDER_LABEL) as HTMLSelectElement).value).toBe("anthropic-key");
      expect(screen.getByText(NR_TOOLS.PROVIDER_PROXY)).toBeTruthy();
      expect(screen.queryByText(NR_TOOLS.PROVIDER_REQUIRED)).toBeNull();
      expect(draft().tools).toEqual([{ id: "claude-code", kind: "harness" }]);
    });

    it("lets a pick replace the server's choice, and says nothing when the server asks for none and chose none", async () => {
      show([CLAUDE_UNGATED], CLAUDE, [MCP_DOCS], { harnessSet: chose("anthropic-key") });
      await userEvent.selectOptions(screen.getByLabelText(NR_TOOLS.PROVIDER_LABEL), "bedrock-sso");
      expect((screen.getByLabelText(NR_TOOLS.PROVIDER_LABEL) as HTMLSelectElement).value).toBe("bedrock-sso");
      expect(draft().tools).toEqual([{ id: "claude-code", kind: "harness", model_provider: "bedrock-sso" }]);
      cleanup();
      show([CLAUDE_UNGATED]);
      expect((screen.getByLabelText(NR_TOOLS.PROVIDER_LABEL) as HTMLSelectElement).value).toBe("");
      expect(screen.queryByText(NR_TOOLS.PROVIDER_REQUIRED)).toBeNull();
    });

    it("ignores a server choice the picker does not list", () => {
      show([CLAUDE_UNGATED], CLAUDE, [MCP_DOCS], { harnessSet: chose("gateway-off"), setup: [CHOOSE] });
      expect((screen.getByLabelText(NR_TOOLS.PROVIDER_LABEL) as HTMLSelectElement).value).toBe("");
      expect(screen.getByText(NR_TOOLS.PROVIDER_REQUIRED)).toBeTruthy();
    });

    it("gives a harness no provider picker when nothing serves it", () => {
      show([CUSTOM], { tools: [{ id: "custom", kind: "harness" }] });
      expect(screen.queryByLabelText(NR_TOOLS.PROVIDER_LABEL)).toBeNull();
    });
  });

  describe("default for a tool with no rule", () => {
    it("greys Run it on the gated lane and says why in text", () => {
      show([CLAUDE_GATED]);
      const run = within(screen.getByRole("radiogroup", { name: NR_TOOLS.DEFAULT_LABEL })).getByRole("radio", { name: NR_TOOLS.EFFECT_ALLOW });
      expect((run as HTMLInputElement).disabled).toBe(true);
      expect(screen.getByText("Your policy asks first for tools with no rule. A run can only be stricter.")).toBeTruthy();
      expect((screen.getByRole("radio", { name: NR_TOOLS.EFFECT_HOLD }) as HTMLInputElement).checked).toBe(true);
      expect(screen.getByText(NR_TOOLS.DEFAULT_HELP_GATED)).toBeTruthy();
    });

    it("says an operator or the person approves, and that Claude Code asks in an attended run", () => {
      h.operator = true;
      show([CLAUDE_GATED]);
      expect(screen.getByText(NR_TOOLS.DEFAULT_HELP_OPERATOR)).toBeTruthy();
      cleanup();
      show([CLAUDE_ATTENDED]);
      expect(screen.getByText(NR_TOOLS.DEFAULT_HELP_ATTENDED)).toBeTruthy();
      expect((screen.getByRole("radio", { name: NR_TOOLS.EFFECT_ALLOW }) as HTMLInputElement).disabled).toBe(true);
    });

    it("sends a stricter default, and Run it as no default at all", async () => {
      show([CLAUDE_UNGATED], { tools: [{ id: "claude-code", kind: "harness", default_effect: "hold" }] });
      expect((screen.getByRole("radio", { name: NR_TOOLS.EFFECT_HOLD }) as HTMLInputElement).checked).toBe(true);
      await userEvent.click(screen.getByRole("radio", { name: NR_TOOLS.EFFECT_DENY }));
      expect(draft().tools[0]).toMatchObject({ default_effect: "deny" });
      await userEvent.click(screen.getByRole("radio", { name: NR_TOOLS.EFFECT_ALLOW }));
      expect(draft().tools[0]).toEqual({ id: "claude-code", kind: "harness" });
    });
  });

  describe("tool rules", () => {
    it("shows the policy's rows locked, with no enforcement note, on any lane (A23 a)", () => {
      show([CLAUDE_GATED_LOCKED]);
      const locked = () => screen.getAllByRole("listitem").filter((li) => li.hasAttribute("data-locked")).map((r) => r.textContent);
      expect(locked()).toEqual(["WebFetchBlock it", "BashAsk first"]);
      cleanup();
      show([CLAUDE_UNGATED_LOCKED]);
      expect(locked()).toEqual(["WebFetchBlock it"]);
      cleanup();
      h.answered = false;
      show([CLAUDE_UNGATED_LOCKED]);
      expect(locked()).toEqual(["WebFetchBlock it"]);
    });

    it("keeps the person's Run it selected while the next answer is out", async () => {
      h.answered = false;
      show([{ ...CLAUDE_UNGATED, default: { ...CLAUDE_UNGATED.default, effective: "hold" } }], { tools: [{ id: "claude-code", kind: "harness", default_effect: "hold" }] });
      await userEvent.click(screen.getByRole("radio", { name: NR_TOOLS.EFFECT_ALLOW }));
      expect((screen.getByRole("radio", { name: NR_TOOLS.EFFECT_ALLOW }) as HTMLInputElement).checked).toBe(true);
    });

    it("adds a personal rule and removes it", async () => {
      show([CLAUDE_UNGATED]);
      await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.ADD_RULE }));
      const dialog = within(screen.getByRole("dialog", { name: NR_TOOLS.RULE_TITLE("Claude Code") }));
      expect(document.activeElement).toBe(dialog.getByLabelText(NR_TOOLS.RULE_TOOL));
      setField(dialog.getByLabelText(NR_TOOLS.RULE_TOOL), "WebFetch");
      await userEvent.click(dialog.getByRole("radio", { name: NR_TOOLS.EFFECT_DENY }));
      await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.RULE_CONFIRM }));
      expect(draft().tools[0]).toMatchObject({ tool_rules: [{ tool: "WebFetch", effect: "deny" }] });
      expect(screen.queryByRole("dialog")).toBeNull();
      await userEvent.click(screen.getByRole("button", { name: `${NR_TOOLS.ROW_REMOVE} WebFetch` }));
      expect(draft().tools[0]).toEqual({ id: "claude-code", kind: "harness" });
    });

    it("says an MCP rule that asks first is the harness's to ask", () => {
      show([CLAUDE_UNGATED], { tools: [{ id: "claude-code", kind: "harness", tool_rules: [{ tool: "mcp__docs__search", effect: "hold" }] }] });
      expect(screen.getByText(NR_TOOLS.MCP_ASKS)).toBeTruthy();
    });
  });

  describe("Codex CLI and a custom harness", () => {
    const BOTH: RunIntent = { tools: [{ id: "codex-cli", kind: "harness" }] };

    it("says what Codex CLI cannot do and greys it", () => {
      show([CODEX], BOTH);
      expect(screen.getByText(NR_TOOLS.CODEX_NOTE)).toBeTruthy();
      const group = within(screen.getByRole("radiogroup", { name: NR_TOOLS.DEFAULT_LABEL }));
      expect((group.getByRole("radio", { name: NR_TOOLS.EFFECT_ALLOW }) as HTMLInputElement).checked).toBe(true);
      expect((group.getByRole("radio", { name: NR_TOOLS.EFFECT_HOLD }) as HTMLInputElement).disabled).toBe(true);
      expect((group.getByRole("radio", { name: NR_TOOLS.EFFECT_DENY }) as HTMLInputElement).disabled).toBe(true);
      expect(screen.getByText("Codex CLI can't pause a tool call for approval.")).toBeTruthy();
    });

    it("turns Codex CLI's rule choices off for its own tools in the Add dialog", async () => {
      show([CODEX], BOTH);
      await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.ADD_RULE }));
      const dialog = within(screen.getByRole("dialog"));
      setField(dialog.getByLabelText(NR_TOOLS.RULE_TOOL), "WebFetch");
      for (const effect of [NR_TOOLS.EFFECT_ALLOW, NR_TOOLS.EFFECT_HOLD, NR_TOOLS.EFFECT_DENY]) {
        expect((dialog.getByRole("radio", { name: effect }) as HTMLInputElement).disabled).toBe(true);
      }
      expect((dialog.getByRole("button", { name: NR_TOOLS.RULE_CONFIRM }) as HTMLButtonElement).disabled).toBe(true);
      setField(dialog.getByLabelText(NR_TOOLS.RULE_TOOL), "mcp__docs__search");
      expect((dialog.getByRole("radio", { name: NR_TOOLS.EFFECT_DENY }) as HTMLInputElement).disabled).toBe(false);
    });

    it("renders a custom harness read-only, with its reason", () => {
      show([CUSTOM], { tools: [{ id: "custom", kind: "harness" }] });
      const c = card("Your own tools");
      expect(c.getByText("Wardyn can't set tool rules for Your own tools.")).toBeTruthy();
      expect(c.queryByRole("radiogroup", { name: NR_TOOLS.DEFAULT_LABEL })).toBeNull();
      expect(c.queryByRole("button", { name: NR_TOOLS.ADD_RULE })).toBeNull();
    });
  });

  describe("MCP servers", () => {
    it("shows each server on or off for the harness, and what to do with none", () => {
      show([CLAUDE_UNGATED, CODEX], { tools: [{ id: "claude-code", kind: "harness" }, { id: "codex-cli", kind: "harness" }] });
      expect((card("Claude Code").getByRole("checkbox", { name: "Docs" })).getAttribute("aria-checked")).toBe("true");
      expect((card("Codex CLI").getByRole("checkbox", { name: "Docs" })).getAttribute("aria-checked")).toBe("false");
      cleanup();
      show([CLAUDE_UNGATED], CLAUDE, []);
      expect(screen.getByText(NR_TOOLS.MCP_NONE)).toBeTruthy();
    });

    it("writes the harnesses a server is turned on for to its component", async () => {
      const intent: RunIntent = { tools: [{ id: "claude-code", kind: "harness" }, { id: "codex-cli", kind: "harness" }], components: [{ id: "mcp-docs" }] };
      show([CLAUDE_UNGATED, CODEX], intent);
      await userEvent.click(card("Codex CLI").getByRole("checkbox", { name: "Docs" }));
      expect(draft().components).toEqual([{ id: "mcp-docs", mcp_harnesses: ["claude-code", "codex-cli"] }]);
      await userEvent.click(card("Claude Code").getByRole("checkbox", { name: "Docs" }));
      expect(draft().components).toEqual([{ id: "mcp-docs", mcp_harnesses: ["codex-cli"] }]);
    });

    it("disables the toggle for a harness Wardyn cannot configure, with the reason", () => {
      show([CUSTOM], { tools: [{ id: "custom", kind: "harness" }] });
      const box = card("Your own tools").getByRole("checkbox", { name: "Docs" });
      expect(box.hasAttribute("disabled")).toBe(true);
      expect(card("Your own tools").getByText("Wardyn can't configure MCP servers for Your own tools.")).toBeTruthy();
    });
  });
});
