/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A3.1 (open egress wording) and A3.2 (a VM names no container barrier) in both Review presentations: the groups and the Effective policy card.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { preview } from "../../../../lib/new-run-fixtures/base";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import { SCENARIO_ACCESS } from "../../../../lib/new-run-fixtures/scenarios/tools";
import type { AccessFacts, AccessHost } from "../../../../lib/types/new-run-access";
import { NR_MACHINE } from "../../../wardyn/copy/new-run/machine";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { initialDraft, type RunIntent } from "../draft/run-intent";
import { EffectivePolicyCard } from "./effective-policy-card";
import { AccessGroup } from "./groups/access-group";
import { WhereGroup } from "./groups/where-group";
import type { ReviewPreflight } from "./review-seams";

afterEach(cleanup);

const host = (name: string): AccessHost => ({ host: name, via: "requested", rows: [], verdict: "allowed", source: { kind: "policy", id: "p", name: "p" } });
const facts = (hosts: AccessHost[], allowAll: boolean): AccessFacts => ({
  ...SCENARIO_ACCESS.member,
  hosts,
  unlisted: allowAll ? { mode: "deny_with_review", allow_all: true, decider: "you" } : { mode: "deny_with_review", allow_all: false, decider: "you" },
});
const many = [host("a.example.com"), host("b.example.com"), host("c.example.com")];
const OPEN_MANY = "3 hosts listed · other public hosts allowed unless blocked";
const OPEN_ONE = "1 host listed · other public hosts allowed unless blocked";
const draftOf = (intent: RunIntent) => ({ ...initialDraft("alice"), intent });
const pre = (access: AccessFacts) => ({ ...SCENARIO_PREFLIGHT.member, access }) as unknown as ReviewPreflight;
const group = (access: AccessFacts) =>
  render(<AccessGroup draft={initialDraft("alice")} pre={pre(access)} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />);
const card = (access: AccessFacts) =>
  render(<EffectivePolicyCard draft={draftOf({ shape: "agent_task" })} pre={pre(access)} preview={null} />);
const networkRow = () => within(document.querySelector("[data-policy-row=network]") as HTMLElement);

describe("Review: open egress (A3.1)", () => {
  it("says the listed hosts and that others are allowed, in the Access group and the card, for many hosts", () => {
    group(facts(many, true));
    expect(screen.getByText(OPEN_MANY)).toBeInTheDocument();
    cleanup();
    card(facts(many, true));
    expect(networkRow().getByText(OPEN_MANY)).toBeInTheDocument();
  });

  it("uses the singular for one host, and the plural form for none", () => {
    group(facts([host("a.example.com")], true));
    expect(screen.getByText(OPEN_ONE)).toBeInTheDocument();
    cleanup();
    card(facts([host("a.example.com")], true));
    expect(networkRow().getByText(OPEN_ONE)).toBeInTheDocument();
    cleanup();
    group(facts([], true));
    expect(screen.getByText("0 hosts listed · other public hosts allowed unless blocked")).toBeInTheDocument();
  });

  it("keeps the plain count when egress is not open", () => {
    group(facts(many, false));
    expect(screen.getByText(NR_REVIEW.POLICY_HOSTS(3))).toBeInTheDocument();
    cleanup();
    card(facts(many, false));
    expect(networkRow().getByText(NR_REVIEW.POLICY_HOSTS(3))).toBeInTheDocument();
  });

  it("reads the preview's allow_all_egress when no access answer has come", () => {
    const spec = { ...preview().spec, allowed_domains: ["a.example.com", "b.example.com"], allow_all_egress: true };
    render(<EffectivePolicyCard draft={draftOf({ shape: "agent_task" })} pre={null} preview={preview({ spec })} />);
    expect(networkRow().getByText("2 hosts listed · other public hosts allowed unless blocked")).toBeInTheDocument();
  });
});

describe("Review: a virtual machine's barrier (A3.2)", () => {
  const where = (intent: RunIntent) =>
    render(<WhereGroup draft={draftOf(intent)} pre={SCENARIO_PREFLIGHT.member} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />);
  const barrierRow = () => within(document.querySelector("[data-policy-row=barrier]") as HTMLElement);

  it.each(["macos_vm", "linux_vm"] as const)("%s: the Where group has no chip, and says Barrier · The virtual machine", (kind) => {
    where({ shape: "agent_task", machine_kind: kind });
    expect(screen.getByTestId("where-vm-barrier")).toHaveTextContent("Barrier · The virtual machine");
    expect(document.querySelector("[data-barrier-chip], [data-testid=barrier-chip]")).toBeNull();
    expect(screen.queryByText("Stronger")).toBeNull();
  });

  it.each(["macos_vm", "linux_vm"] as const)("%s: the card's Barrier row is The virtual machine and the pool, with no chip and no Default", (kind) => {
    render(<EffectivePolicyCard draft={draftOf({ shape: "agent_task", machine_kind: kind })} pre={SCENARIO_PREFLIGHT.member} preview={null} />);
    expect(barrierRow().getByText(NR_MACHINE.BARRIER_VM)).toBeInTheDocument();
    expect(barrierRow().getByText(NR_REVIEW.WHERE_VALUE("General"))).toBeInTheDocument();
    expect(barrierRow().queryByText(/Default/)).toBeNull();
    expect(barrierRow().queryByText("Wall")).toBeNull();
  });

  it("a container keeps the chip and its detail", () => {
    where({ shape: "agent_task" });
    expect(screen.queryByTestId("where-vm-barrier")).toBeNull();
    cleanup();
    render(<EffectivePolicyCard draft={draftOf({ shape: "agent_task" })} pre={SCENARIO_PREFLIGHT.member} preview={null} />);
    expect(barrierRow().getByText("Wall")).toBeInTheDocument();
    expect(barrierRow().queryByText(NR_MACHINE.BARRIER_VM)).toBeNull();
  });
});
