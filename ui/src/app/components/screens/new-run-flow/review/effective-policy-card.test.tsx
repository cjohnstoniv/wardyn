/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ADO_ACCESS, adoCapName } from "../../../../lib/ado-access-copy";
import { ADO_ENTRA_EDITOR } from "../../../../lib/workspace-providers-copy";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_TOOLS } from "../../../wardyn/copy/new-run/tools";
import { NR_RUN } from "../../../wardyn/copy/new-run/run";
import { PUSH } from "../../../wardyn/copy/push";
import { preview } from "../../../../lib/new-run-fixtures/base";
import type { PushRulesSpec } from "../../../../lib/types/policy";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import { initialDraft, type NewRunDraft, type RunIntent } from "../draft/run-intent";
import { EffectivePolicyCard } from "./effective-policy-card";
import { AccessGroup } from "./groups/access-group";
import { CLAUDE_UNGATED } from "../../../../lib/new-run-fixtures/scenarios/tools-harness";
import { SCENARIO_ACCESS } from "../../../../lib/new-run-fixtures/scenarios/tools";
import type { ReviewPreflight } from "./review-seams";
import { changedRows } from "./policy-rows";
import type { PolicyPreviewResult } from "../../../../lib/types/policy-preview";

// The roster the Tools group names harnesses from: the card names them the same way.
vi.mock("../../../../lib/api/setup", async (orig) => ({
  ...(await orig<typeof import("../../../../lib/api/setup")>()),
  setup: {
    getSetupStatus: async () => ({
      harnesses: [
        { id: "claude-code", display: "Claude Code" },
        { id: "codex-cli", display: "Codex" },
      ],
    }),
  },
}));

afterEach(cleanup);

const BASE: RunIntent = { shape: "agent_task", inline_policy: { allowed_domains: ["pypi.org"] }, tools: [{ id: "claude-code", kind: "harness" }] };
const draftOf = (intent: RunIntent, base: RunIntent = BASE): NewRunDraft => ({ ...initialDraft("alice"), intent, base });
const rows = () => Array.from(document.querySelectorAll("[data-policy-row]")).map((r) => r.getAttribute("data-policy-row"));
const toggle = () => screen.getByRole("switch", { name: NR_REVIEW.ONLY_CHANGED });

describe("Effective policy card", () => {
  it("shows what the sandbox can do, one row per part, with the barrier by name", () => {
    render(<EffectivePolicyCard draft={draftOf(BASE)} pre={SCENARIO_PREFLIGHT.member} preview={null} />);
    expect(rows()).toEqual(["barrier", "credentials", "files", "tools"]);
    const barrier = within(document.querySelector("[data-policy-row=barrier]") as HTMLElement);
    expect(barrier.getByText("Wall")).toBeInTheDocument();
    expect(barrier.getByText(`${NR_RUN.BADGE_DEFAULT} · ${NR_REVIEW.WHERE_VALUE("General")}`)).toBeInTheDocument();
  });

  it.each([
    ["the organisation's pick", BASE, { ...SCENARIO_PREFLIGHT.member, barrier_options: [{ class: "CC2" as const, available: true, default: true, default_source: "organisation" as const }] }, NR_REVIEW.PROV_ORG_DEFAULT],
    ["the person's own choice", { ...BASE, confinement_class: "CC2" as const }, SCENARIO_PREFLIGHT.member, NR_REVIEW.PROV_SELECTED],
  ])("says where the barrier came from: %s", (_name, intent, pre, how) => {
    render(<EffectivePolicyCard draft={draftOf(intent)} pre={pre} preview={null} />);
    expect(screen.getByText(`${how} · ${NR_REVIEW.WHERE_VALUE("General")}`)).toBeInTheDocument();
  });

  it("with Only what I changed on and nothing changed, says so and offers Show everything", async () => {
    const user = userEvent.setup();
    render(<EffectivePolicyCard draft={draftOf(BASE)} pre={null} preview={null} />);
    await user.click(toggle());
    expect(toggle()).toHaveAttribute("aria-checked", "true");
    expect(screen.getByText(NR_REVIEW.NOTHING_CHANGED)).toBeInTheDocument();
    expect(rows()).toEqual([]);
    await user.click(screen.getByRole("button", { name: NR_REVIEW.SHOW_EVERYTHING }));
    expect(toggle()).toHaveAttribute("aria-checked", "false");
    expect(rows().length).toBeGreaterThan(0);
  });

  it("with it on, keeps only the rows the person changed from where the draft started", async () => {
    const user = userEvent.setup();
    const intent: RunIntent = { ...BASE, confinement_class: "CC3", inline_policy: { allowed_domains: ["pypi.org", "example.org"] }, tool_approvals: "hold" };
    render(<EffectivePolicyCard draft={draftOf(intent)} pre={SCENARIO_PREFLIGHT.governed} preview={null} />);
    await user.click(toggle());
    expect(rows()).toEqual(["barrier", "tools"]);
    expect(screen.queryByText(NR_REVIEW.NOTHING_CHANGED)).toBeNull();
  });

  it("counts a per-harness provider, rule or default as a change to credentials or tools", () => {
    const withTool = (patch: object): RunIntent => ({ ...BASE, tools: [{ id: "claude-code", kind: "harness", ...patch }] });
    expect([...changedRows(withTool({ model_provider: "bedrock" }), BASE)]).toEqual(["credentials"]);
    expect([...changedRows(withTool({ default_effect: "hold" }), BASE)]).toEqual(["tools"]);
    expect([...changedRows(BASE, BASE)]).toEqual([]);
  });

  it.each([
    ["an unattended run with a review rule", "background", { require_review_paths: ["infra/**"] }, true],
    ["an interactive run with a review rule", "interactive", { require_review_paths: ["infra/**"] }, false],
    ["an unattended run with only a deny rule", "background", { deny_paths: ["secrets/**"] }, false],
    ["an unattended run with no push rules", "background", undefined, false],
  ] as [string, RunIntent["experience"], PushRulesSpec | undefined, boolean][])("says a held push is refused for %s only when nobody can answer it", (_name, experience, pushRules, shown) => {
    const spec = { ...preview().spec, push_rules: pushRules };
    render(<EffectivePolicyCard draft={draftOf({ ...BASE, experience })} pre={null} preview={preview({ spec })} />);
    expect(screen.queryByText(PUSH.RAIL_UNATTENDED) !== null).toBe(shown);
  });

  it("says in one line what the run's policy grants on Azure DevOps, and nothing for a policy that names none", () => {
    const spec = { ...preview().spec, azure_devops_capabilities: ["code_read", "policy_admin"] };
    const view = render(<EffectivePolicyCard draft={draftOf(BASE)} pre={null} preview={preview({ spec })} />);
    const line = within(screen.getByRole("complementary", { name: NR_REVIEW.POLICY_TITLE })).getByTestId("ado-access-summary");
    // A labelled row like the rows above it, not a loose text-xs line.
    expect(line).toHaveTextContent(`${ADO_ACCESS.SECTION_TITLE}${adoCapName("code_read")} · ${adoCapName("policy_admin")}`);
    expect(line.querySelector(".label-eyebrow")).toHaveTextContent(ADO_ACCESS.SECTION_TITLE);
    expect(within(line).getByText(ADO_ENTRA_EDITOR.HIGH_RISK_BADGE)).toBeInTheDocument();
    view.rerender(<EffectivePolicyCard draft={draftOf(BASE)} pre={null} preview={preview()} />);
    expect(screen.queryByTestId("ado-access-summary")).not.toBeInTheDocument();
  });

  it("reads a held tool default as Hold for approval", () => {
    render(<EffectivePolicyCard draft={draftOf({ ...BASE, tool_approvals: "hold" })} pre={null} preview={null} />);
    expect(screen.getByText(NR_REVIEW.POLICY_HOLD)).toBeInTheDocument();
    expect(screen.queryByText(NR_REVIEW.POLICY_AUTOMATIC)).toBeNull();
  });

  const harnessDefault = (harness: string, effective: string) =>
    ({ ...CLAUDE_UNGATED, harness, default: { ...CLAUDE_UNGATED.default, effective } });
  const withDefaults = (...d: [string, string][]) => ({ ...SCENARIO_ACCESS.member, harnesses: d.map(([h, e]) => harnessDefault(h, e)) });

  it("reads an effective deny tool default as Block it, not Run automatically (DR-0244)", () => {
    const access = withDefaults(["claude-code", "deny"]) as never;
    render(<EffectivePolicyCard draft={draftOf(BASE)} pre={{ ...SCENARIO_PREFLIGHT.member, access } as never} preview={null} />);
    const tools = within(document.querySelector("[data-policy-row=tools]") as HTMLElement);
    expect(tools.getByText(NR_TOOLS.EFFECT_DENY)).toBeInTheDocument();
    expect(tools.queryByText(NR_REVIEW.POLICY_AUTOMATIC)).toBeNull();
  });

  it("names each harness when the included harnesses default differently (DR-0244)", async () => {
    const access = withDefaults(["claude-code", "hold"], ["codex-cli", "deny"]) as never;
    render(<EffectivePolicyCard draft={draftOf(BASE)} pre={{ ...SCENARIO_PREFLIGHT.member, access } as never} preview={null} />);
    const tools = document.querySelector("[data-policy-row=tools]") as HTMLElement;
    await waitFor(() => expect(tools).toHaveTextContent(`Claude Code: ${NR_REVIEW.POLICY_HOLD} · Codex: ${NR_TOOLS.EFFECT_DENY}`));
    expect(tools.textContent).toContain(NR_REVIEW.POLICY_TOOLS_OF("Claude Code", NR_REVIEW.POLICY_HOLD));
  });

  it("falls back to the harness id until the roster answers, and keeps one value with its source when the defaults agree", async () => {
    const mixed = withDefaults(["claude-code", "allow"], ["codex-cli", "deny"]) as never;
    const view = render(<EffectivePolicyCard draft={draftOf(BASE)} pre={{ ...SCENARIO_PREFLIGHT.member, access: mixed } as never} preview={null} />);
    expect(document.querySelector("[data-policy-row=tools]")).toHaveTextContent(`claude-code: ${NR_REVIEW.POLICY_AUTOMATIC} · codex-cli: ${NR_TOOLS.EFFECT_DENY}`);
    view.unmount();
    const source = { kind: "policy", name: "Team policy" };
    const held = (harness: string) => ({ ...harnessDefault(harness, "hold"), default: { ...CLAUDE_UNGATED.default, effective: "hold", source } });
    const same = { ...SCENARIO_ACCESS.member, harnesses: [held("claude-code"), held("codex-cli")] } as never;
    render(<EffectivePolicyCard draft={draftOf(BASE)} pre={{ ...SCENARIO_PREFLIGHT.member, access: same } as never} preview={null} />);
    const tools = document.querySelector("[data-policy-row=tools]") as HTMLElement;
    expect(tools).toHaveTextContent(NR_REVIEW.POLICY_HOLD);
    expect(tools).not.toHaveTextContent("claude-code:");
    expect(tools).not.toHaveTextContent("Claude Code:");
    expect(tools).toHaveTextContent("Team policy");
  });

  it("says what an interactive Claude Code default that accepts file edits does, and nothing for another source", () => {
    const withSource = (source: { kind: string; id?: string }) =>
      ({ ...SCENARIO_ACCESS.member, harnesses: [{ ...harnessDefault("claude-code", "hold"), default: { ...CLAUDE_UNGATED.default, effective: "hold", source } }] }) as never;
    const view = render(<EffectivePolicyCard draft={draftOf(BASE)} pre={{ ...SCENARIO_PREFLIGHT.member, access: withSource({ kind: "harness", id: "accept_edits" }) } as never} preview={null} />);
    expect(document.querySelector("[data-policy-row=tools]")).toHaveTextContent(NR_REVIEW.POLICY_ACCEPTS_EDITS);
    view.unmount();
    render(<EffectivePolicyCard draft={draftOf(BASE)} pre={{ ...SCENARIO_PREFLIGHT.member, access: withSource({ kind: "harness" }) } as never} preview={null} />);
    expect(document.querySelector("[data-policy-row=tools]")).not.toHaveTextContent(NR_REVIEW.POLICY_ACCEPTS_EDITS);
  });

  // #2822: the tools row says each harness's own walls; one text when they agree, each harness's when they differ.
  it("says the harness's inner walls on the tools row, per harness when they differ", () => {
    const walled = (harness: string, inner_walls: object) => ({ ...harnessDefault(harness, "hold"), inner_walls });
    const access = (...h: object[]) => ({ ...SCENARIO_ACCESS.member, harnesses: h }) as never;
    const gated = "Inner walls: harness sandbox off, approvals in Wardyn";
    const view = render(<EffectivePolicyCard draft={draftOf(BASE)} pre={{ ...SCENARIO_PREFLIGHT.member, access: access(walled("claude-code", { sandbox_mode: "off", approvals: "wardyn" })) } as never} preview={null} />);
    expect(document.querySelector("[data-policy-row=tools]")).toHaveTextContent(gated);
    view.unmount();
    render(<EffectivePolicyCard draft={draftOf(BASE)} pre={{ ...SCENARIO_PREFLIGHT.member,
      access: access(walled("claude-code", { sandbox_mode: "off", approvals: "harness" }), walled("codex-cli", { approvals: "harness" })) } as never} preview={null} />);
    const tools = document.querySelector("[data-policy-row=tools]") as HTMLElement;
    expect(tools).toHaveTextContent("claude-code: Inner walls: harness sandbox off, approvals in the harness");
    expect(tools).toHaveTextContent("codex-cli: Inner walls: the harness's own sandbox, approvals in the harness");
  });

  it.each([[["pypi.org"], NR_REVIEW.POLICY_HOST_ONE], [["pypi.org", "registry.npmjs.org"], NR_REVIEW.POLICY_HOSTS(2)]])(
    "counts the allowed hosts with a singular for one (%j)", (hosts, text) => {
      const preview = { spec: { allowed_domains: hosts }, source: { name: "Source" } } as unknown as PolicyPreviewResult;
      render(<EffectivePolicyCard draft={draftOf(BASE)} pre={null} preview={preview} />);
      expect(screen.getByText(text)).toBeInTheDocument();
    },
  );

  it("shows Network and the Access value whenever the server's access facts carry hosts, and the barrier with its strength word", () => {
    const one = SCENARIO_ACCESS.member.hosts[0];
    const access = { ...SCENARIO_ACCESS.member, hosts: [one, { ...one, host: "pypi.org" }] };
    const pre = { ...SCENARIO_PREFLIGHT.member, access } as ReviewPreflight;
    render(<EffectivePolicyCard draft={draftOf(BASE)} pre={pre} preview={null} />);
    const network = document.querySelector('[data-policy-row="network"]') as HTMLElement;
    expect(within(network).getByText(NR_REVIEW.POLICY_HOSTS(2))).toBeInTheDocument();
    const barrier = document.querySelector('[data-policy-row="barrier"]') as HTMLElement;
    expect(within(barrier).getByText(NR_RUN.TAGLINE_WALL)).toBeInTheDocument();
    cleanup();
    render(<AccessGroup draft={draftOf(BASE)} pre={pre} preview={null} docRows={[]} go={() => {}} dispatch={() => {}} />);
    expect(screen.getByText(NR_REVIEW.POLICY_HOSTS(2))).toBeInTheDocument();
  });
});
