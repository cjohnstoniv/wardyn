/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { adoCapName } from "../../../../../lib/ado-access-copy";
import { ADO_PAT } from "../../../../../lib/ado-pat-copy";
import type { SCMAccessPAT } from "../../../../../lib/types/ado-pat";
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

  it("names a credential that is not a model's by the server's label for its kind", () => {
    render(<Totals names={HARNESS_NAMES} access={MEMBER} answered />);
    expect(credentials().getByText("Your organisation's GitHub App")).toBeInTheDocument();
  });

  it("names a model credential's provider once known, and says Model provider until then", () => {
    render(<Totals names={HARNESS_NAMES} access={MEMBER} answered />);
    expect(credentials().getByText(NR_TOOLS.PROVIDER_LABEL, { exact: false })).toBeInTheDocument();
    cleanup();
    render(<Totals names={HARNESS_NAMES} access={MEMBER} answered providers={new Map([["claude-code", "Bedrock (team)"]])} />);
    expect(credentials().getByText("Bedrock (team)", { exact: false })).toBeInTheDocument();
    expect(credentials().queryByText(NR_TOOLS.PROVIDER_LABEL, { exact: false })).toBeNull();
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

  // #2541: the two rows carry the same provider name, so the resident token is named and both say they outlive the run.
  it("with the switch off, names the resident SSO token apart from the role credentials and says neither ends with the run", () => {
    render(<Totals names={HARNESS_NAMES} access={BEDROCK_SSO_SWITCH_OFF} answered providers={new Map([["claude-code", "Bedrock (team)"]])} />);
    expect(credentials().getAllByText("Bedrock (team)", { exact: false })).toHaveLength(2);
    expect(credentials().getAllByText(`Bedrock (team) ${NR_TOOLS.SSO_TOKEN}`, { exact: false })).toHaveLength(1);
    expect(credentials().getAllByText(NR_TOOLS.EXPIRES_OWN)).toHaveLength(2);
  });

  it("a credential that ends with the run carries no expiry sentence", () => {
    render(<Totals names={HARNESS_NAMES} access={MEMBER} answered />);
    expect(credentials().queryByText(NR_TOOLS.EXPIRES_OWN)).not.toBeInTheDocument();
  });
});

describe("Credentials total: a run whose Azure DevOps token Wardyn creates", () => {
  // A repository on the Azure DevOps sign-in lane, as the server lists it: its row and its credential.
  const WITH_ADO: AccessFacts = {
    ...MEMBER,
    rows: [...MEMBER.rows, { id: "repo:ado", kind: "repository", from: "repository", name: "contoso", status: "ready", hosts: [], credentials: ["repo:ado:credential"], source: { kind: "workspace" } }],
    credentials: [...MEMBER.credentials, { id: "repo:ado:credential", row: "repo:ado", kind: "ado_signin", owner: "own", delivery: "proxy", after_end: "gone" }],
  };
  const scm = (over: Partial<SCMAccessPAT>) => ({ token_mode: "minted_pat", state: "live", default_profile: ["code_read"], ...over }) as SCMAccessPAT;
  const facts = (access: SCMAccessPAT | undefined, policyCaps?: unknown, onConnect = vi.fn()) => ({ access, policyCaps, connecting: false, onConnect });

  it("says on the credential's own row what the run's token carries: the policy's access, else the row's default", () => {
    const view = render(<Totals names={HARNESS_NAMES} access={WITH_ADO} answered ado={facts(scm({}), ["code_read", "code_write"])} />);
    const line = credentials().getByTestId("ado-run-token-line");
    expect(line).toHaveTextContent(`${ADO_PAT.NEWRUN_LINE_PREFIX}${["code_read", "code_write"].map(adoCapName).join(", ")}${ADO_PAT.NEWRUN_LINE_SUFFIX}`);
    expect(line.closest("li")).toHaveTextContent(NR_TOOLS.FOR_ROW("contoso"));
    view.rerender(<Totals names={HARNESS_NAMES} access={WITH_ADO} answered ado={facts(scm({}))} />);
    expect(credentials().getByTestId("ado-run-token-line")).toHaveTextContent(`${ADO_PAT.NEWRUN_LINE_PREFIX}${adoCapName("code_read")}${ADO_PAT.NEWRUN_LINE_SUFFIX}`);
    expect(credentials().queryByTestId("ado-launch-note")).not.toBeInTheDocument();
  });

  it("not connected: a row of the card says to connect first, and Connect starts it", async () => {
    const onConnect = vi.fn();
    render(<Totals names={HARNESS_NAMES} access={WITH_ADO} answered ado={facts(scm({ state: "not_configured" }), undefined, onConnect)} />);
    const note = credentials().getByTestId("ado-launch-note");
    expect(note.closest("li")).not.toBeNull();
    expect(within(note).getByText(ADO_PAT.LAUNCH_NOT_CONNECTED)).toBeInTheDocument();
    await userEvent.click(within(note).getByRole("button", { name: ADO_PAT.MEMBER_CONNECT }));
    expect(onConnect).toHaveBeenCalledTimes(1);
  });

  it("says neither on the sign-in lane, nor for a run that carries no Azure DevOps credential", () => {
    const view = render(<Totals names={HARNESS_NAMES} access={WITH_ADO} answered ado={facts(scm({ token_mode: "bearer", state: "not_configured" }))} />);
    expect(screen.queryByTestId("ado-run-token-line")).not.toBeInTheDocument();
    expect(screen.queryByTestId("ado-launch-note")).not.toBeInTheDocument();
    // No token is created for a run with no Azure DevOps repository, so nothing claims one.
    view.rerender(<Totals names={HARNESS_NAMES} access={MEMBER} answered ado={facts(scm({ state: "not_configured" }))} />);
    expect(screen.queryByTestId("ado-run-token-line")).not.toBeInTheDocument();
    expect(screen.queryByTestId("ado-launch-note")).not.toBeInTheDocument();
  });
});
