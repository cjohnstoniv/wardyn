/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { BEDROCK_SSO_SWITCH_OFF, BEDROCK_SSO_SWITCH_ON, HARNESS_NAMES, SCENARIO_ACCESS } from "../../../../../lib/new-run-fixtures/scenarios/tools";
import type { AccessFacts, AccessHost } from "../../../../../lib/types/new-run-access";
import { Totals, unlistedSentences } from "./totals";

afterEach(cleanup);

const MEMBER = SCENARIO_ACCESS.member;
const BEDROCK = "bedrock-runtime.us-east-1.amazonaws.com";
const withHost = (over: Partial<AccessHost>): AccessFacts => ({ ...MEMBER, hosts: [{ ...MEMBER.hosts[0], ...over }] });
const network = () => within(screen.getByRole("heading", { name: NR_TOOLS.NETWORK_TITLE }).parentElement as HTMLElement);
const credentials = () => within(screen.getByRole("heading", { name: NR_TOOLS.CREDENTIALS_TITLE }).parentElement as HTMLElement);

describe("Network total: Requested until checked", () => {
  it("shows a host as Requested, never Allowed, before the server has answered this body", () => {
    render(<Totals names={HARNESS_NAMES} access={MEMBER} answered={false} />);
    expect(network().getByText(BEDROCK)).toBeInTheDocument();
    expect(network().getByText(NR_TOOLS.NET_REQUESTED)).toBeInTheDocument();
    expect(network().queryByText(NR_TOOLS.NET_ALLOWED)).not.toBeInTheDocument();
  });

  it("keeps even a not-allowed verdict back as Requested until the answer is for this body", () => {
    render(<Totals names={HARNESS_NAMES} access={withHost({ verdict: "not_allowed", reason: "clamped" })} answered={false} />);
    expect(network().getByText(NR_TOOLS.NET_REQUESTED)).toBeInTheDocument();
    expect(network().queryByText(NR_TOOLS.NET_NOT_ALLOWED)).not.toBeInTheDocument();
  });

  it.each([
    ["allowed", NR_TOOLS.NET_ALLOWED],
    ["needs_approval", NR_TOOLS.NET_NEEDS_APPROVAL],
    ["not_allowed", NR_TOOLS.NET_NOT_ALLOWED],
  ])("shows the %s verdict once the server has answered", (verdict, text) => {
    render(<Totals names={HARNESS_NAMES} access={withHost({ verdict })} answered />);
    expect(network().getByText(text)).toBeInTheDocument();
    expect(network().queryByText(NR_TOOLS.NET_REQUESTED)).not.toBeInTheDocument();
  });

  it("reads a verdict it does not know as Requested", () => {
    render(<Totals names={HARNESS_NAMES} access={withHost({ verdict: "future_verdict" })} answered />);
    expect(network().getByText(NR_TOOLS.NET_REQUESTED)).toBeInTheDocument();
  });

  it("names the row each host comes from, and none for a host only the policy brings", () => {
    render(<Totals names={HARNESS_NAMES} access={SCENARIO_ACCESS.app} answered />);
    expect(network().getByText("registry.npmjs.org")).toBeInTheDocument();
    expect(network().queryByText(/^from /)).not.toBeInTheDocument();
    cleanup();
    render(<Totals names={HARNESS_NAMES} access={MEMBER} answered />);
    expect(network().getByText(NR_TOOLS.FROM_ROW("Claude Code"), { exact: false })).toBeInTheDocument();
  });

  it("says nothing is listed only once the server has answered", () => {
    const empty: AccessFacts = { ...MEMBER, hosts: [], credentials: [] };
    render(<Totals names={HARNESS_NAMES} access={empty} answered={false} />);
    expect(screen.queryByText(NR_TOOLS.NO_HOSTS)).not.toBeInTheDocument();
    cleanup();
    render(<Totals names={HARNESS_NAMES} access={empty} answered />);
    expect(screen.getByText(NR_TOOLS.NO_HOSTS)).toBeInTheDocument();
    expect(screen.getByText(NR_TOOLS.NO_CREDENTIALS)).toBeInTheDocument();
  });

  it("shows no lists and no policy line before any answer", () => {
    render(<Totals names={HARNESS_NAMES} access={undefined} answered={false} />);
    expect(screen.queryByText(NR_TOOLS.NO_HOSTS)).not.toBeInTheDocument();
    expect(screen.queryByText(NR_TOOLS.NET_POLICY_CEILING)).not.toBeInTheDocument();
  });
});

describe("the unlisted-host line", () => {
  const u = (over: Partial<AccessFacts["unlisted"]>): AccessFacts["unlisted"] => ({ mode: "always_deny", allow_all: false, decider: "you", ...over });

  it.each([
    [u({ mode: "deny_with_review", decider: "you" }), NR_TOOLS.NET_POLICY_DENY_REVIEW_YOU],
    [u({ mode: "deny_with_review", decider: "admin" }), NR_TOOLS.NET_POLICY_DENY_REVIEW_ADMIN],
    [u({ mode: "always_deny" }), NR_TOOLS.NET_POLICY_ALWAYS_DENY],
    [u({ mode: "wait_for_review", decider: "you" }), NR_TOOLS.NET_POLICY_HELD_YOU],
    [u({ mode: "wait_for_review", decider: "admin" }), NR_TOOLS.NET_POLICY_HELD_ADMIN],
    [u({ mode: "always_deny", allow_all: true }), NR_TOOLS.NET_POLICY_ALLOW_ALL],
  ])("picks the sentence for %j", (unlisted, sentence) => {
    expect(unlistedSentences(unlisted, true)[0]).toBe(sentence);
  });

  it("adds the hold timeout to a held mode, and not to a refused one", () => {
    expect(unlistedSentences(u({ mode: "wait_for_review", hold_sec: 30 }), true)).toContain(NR_TOOLS.NET_POLICY_TIMEOUT(30));
    expect(unlistedSentences(u({ mode: "always_deny", hold_sec: 30 }), true)).not.toContain(NR_TOOLS.NET_POLICY_TIMEOUT(30));
  });

  it("says only the ceiling line while the answer is stale, never the last answer's mode", () => {
    expect(unlistedSentences(u({ allow_all: true }), false)).toEqual([NR_TOOLS.NET_POLICY_CEILING]);
  });

  it("names the ceiling until the check, and the check after it", () => {
    expect(unlistedSentences(u({}), false).at(-1)).toBe(NR_TOOLS.NET_POLICY_CEILING);
    expect(unlistedSentences(u({}), true).at(-1)).toBe(NR_TOOLS.NET_POLICY_CHECKED);
  });

  it("renders the sentences under the host list", () => {
    render(<Totals names={HARNESS_NAMES} access={SCENARIO_ACCESS.admin} answered />);
    expect(network().getByText(new RegExp(NR_TOOLS.NET_POLICY_TIMEOUT(30)))).toBeInTheDocument();
  });
});

describe("Credentials total", () => {
  it("labels each credential with its delivery, from the row it is for", () => {
    render(<Totals names={HARNESS_NAMES} access={MEMBER} answered />);
    expect(credentials().getByText(NR_TOOLS.DELIVERY_PROXY)).toBeInTheDocument();
    expect(credentials().getByText(NR_TOOLS.DELIVERY_GIT_BROKER)).toBeInTheDocument();
    expect(credentials().getByText(NR_TOOLS.FOR_ROW("Claude Code"))).toBeInTheDocument();
    expect(credentials().getByText(NR_TOOLS.FOR_ROW("acme/web"))).toBeInTheDocument();
    expect(credentials().getByText(NR_TOOLS.CREDENTIALS_POLICY)).toBeInTheDocument();
  });

  it("shows no row name for a credential whose row is unknown, never its id", () => {
    const orphan: AccessFacts = { ...MEMBER, credentials: [{ ...MEMBER.credentials[0], row: "agent:unlisted" }] };
    render(<Totals names={HARNESS_NAMES} access={orphan} answered />);
    expect(credentials().queryByText(/agent:unlisted/)).not.toBeInTheDocument();
  });

  it("with the proxy-inject switch on, a Bedrock SSO credential is one short-lived line Wardyn renews", () => {
    render(<Totals names={HARNESS_NAMES} access={BEDROCK_SSO_SWITCH_ON} answered />);
    expect(credentials().getAllByText(NR_TOOLS.DELIVERY_SHORT_LIVED)).toHaveLength(1);
    expect(credentials().queryByText(NR_TOOLS.DELIVERY_FILE)).not.toBeInTheDocument();
    expect(credentials().getByText(NR_TOOLS.CREDENTIALS_POLICY)).toBeInTheDocument();
  });

  it("with the switch off, names both the role credentials and the resident SSO access token, and does not claim a renewal", () => {
    render(<Totals names={HARNESS_NAMES} access={BEDROCK_SSO_SWITCH_OFF} answered />);
    expect(credentials().getAllByText(NR_TOOLS.FOR_ROW("Claude Code"))).toHaveLength(2);
    expect(credentials().getByText(NR_TOOLS.DELIVERY_SHORT_LIVED)).toBeInTheDocument();
    expect(credentials().getByText(NR_TOOLS.DELIVERY_FILE)).toBeInTheDocument();
    expect(credentials().queryByText(NR_TOOLS.CREDENTIALS_POLICY)).not.toBeInTheDocument();
  });
});
