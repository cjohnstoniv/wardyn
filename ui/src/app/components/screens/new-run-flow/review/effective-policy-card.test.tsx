/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_RUN } from "../../../wardyn/copy/new-run/run";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import { initialDraft, type NewRunDraft, type RunIntent } from "../draft/run-intent";
import { EffectivePolicyCard } from "./effective-policy-card";
import { changedRows } from "./policy-rows";

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

  it("reads a held tool default as Hold for approval", () => {
    render(<EffectivePolicyCard draft={draftOf({ ...BASE, tool_approvals: "hold" })} pre={null} preview={null} />);
    expect(screen.getByText(NR_REVIEW.POLICY_HOLD)).toBeInTheDocument();
    expect(screen.queryByText(NR_REVIEW.POLICY_AUTOMATIC)).toBeNull();
  });
});
