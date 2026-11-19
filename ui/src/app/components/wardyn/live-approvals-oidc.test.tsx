/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The held-run sign-in row for an endpoint that signs people in. Its request names its provider row by uid, which
// setup status maps to the provider: the door is that provider's, never the run's own provider's (a second harness
// has its own), and a uid with no match gets the generic wording and no door. The AWS wording is never used for it.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";
import { makeApproval } from "../../../test/factories";
import { OperatorProvider } from "./operator-context";
import { ModelAccessProvider } from "./model-access-context";
import { OIDC_SIGN_IN as T } from "./copy/oidc-signin";
import { OIDC_HOLD_LANE, REAUTH_HEADING, REAUTH_ROW, oidcHoldProvider, reauthAudience, reauthRowHint } from "./model-access-copy";
import { MODEL_PROVIDERS, providerStatus } from "../../lib/test-fixtures";
import type { SetupModelProvider } from "../../lib/types";

const listApprovalsMock = vi.fn((..._a: unknown[]): Promise<ApprovalRequest[]> => Promise.resolve([]));
vi.mock("../../lib/api/approvals", () => ({
  approvals: { listApprovals: (...a: unknown[]) => listApprovalsMock(...a), approve: vi.fn(), deny: vi.fn() },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));
vi.mock("../screens/settings/harness-login-pane", () => ({
  HarnessLoginPane: () => <div data-testid="fake-pane" />,
}));

import { LiveApprovals } from "./live-approvals";

const SSO: SetupModelProvider = {
  id: "corp-sso",
  name: "Corp SSO",
  kind: "custom_endpoint",
  harnesses: ["claude-code"],
  host: "llm.contoso.example",
  sign_in: "oidc",
  uid: "uid-a",
};
const OTHER: SetupModelProvider = { ...SSO, id: "other-sso", name: "Other SSO", host: "llm.other.example", uid: "uid-b" };
const hold = (over: Partial<ApprovalRequest> = {}): ApprovalRequest =>
  makeApproval({
    id: "oidc-1",
    run_id: "r1",
    kind: "credential_reauth",
    requested_scope: { lane: OIDC_HOLD_LANE, mechanism: "ado_entra", reason: "x", owner: "alice@corp", provider_id: "uid-a" },
    state: "PENDING",
    requested_at: new Date().toISOString(),
    ...over,
  });

function mount(principal: string, runProvider: string | null = "corp-sso", rows: SetupModelProvider[] = [SSO]) {
  const status = providerStatus(rows.map((provider) => ({ provider, state: "expired_signin" })));
  const run = { created_by: "alice@corp", state: "RUNNING", agents: [], model_provider_id: runProvider ?? undefined };
  return render(
    <MemoryRouter>
      <OperatorProvider operator={false} securityOperator={false} principal={principal}>
        <ModelAccessProvider status={status} onRefresh={() => {}}>
          <LiveApprovals runId="r1" run={run as never} />
        </ModelAccessProvider>
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => listApprovalsMock.mockReset().mockResolvedValue([hold()]));

describe("the audience", () => {
  it("reads the lane and the hold's own uid, and resolves the provider only from that uid and only when it signs people in", () => {
    const a = reauthAudience(hold(), { operator: false, principal: "alice@corp" });
    expect(a.oidc).toBe(true);
    expect(a.provider).toBe("");
    expect(a.holdUID).toBe("uid-a");
    const status = providerStatus([{ provider: SSO }, { provider: OTHER }]);
    expect(oidcHoldProvider(a, status)?.id).toBe("corp-sso");
    const b = reauthAudience(hold({ requested_scope: { ...hold().requested_scope, provider_id: "uid-b" } }), { operator: false, principal: "alice@corp" });
    expect(oidcHoldProvider(b, status)?.id).toBe("other-sso");
    const gone = reauthAudience(hold({ requested_scope: { ...hold().requested_scope, provider_id: "uid-gone" } }), { operator: false, principal: "alice@corp" });
    expect(oidcHoldProvider(gone, status)).toBeUndefined();
    expect(oidcHoldProvider(a, providerStatus([{ provider: { ...SSO, sign_in: undefined } }]))).toBeUndefined();
    expect(oidcHoldProvider(a, null)).toBeUndefined();
    const aws = reauthAudience(hold({ requested_scope: { provider: "bedrock-prod", owner: "alice@corp" } }), { operator: false, principal: "alice@corp" });
    expect(aws.oidc).toBe(false);
    expect(oidcHoldProvider(aws, status)).toBeUndefined();
  });

  it("tells another person whose sign-in is awaited", () => {
    const a = reauthAudience(hold(), { operator: false, principal: "bob@corp" });
    expect(reauthRowHint(a)).toBe(T.APPROVAL_OTHER("alice@corp"));
  });
});

describe("the strip row", () => {
  it("is provider-named: the label, the heading and a button that names the provider, never AWS", async () => {
    mount("alice@corp");
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText(T.APPROVAL_LABEL("Corp SSO"))).toBeInTheDocument();
    expect(within(panel).getByText(T.APPROVAL_HEADING("Corp SSO"))).toBeInTheDocument();
    expect(within(panel).getByRole("button", { name: T.BUTTON("Corp SSO") })).toBeInTheDocument();
    expect(within(panel).queryByText(REAUTH_ROW.label)).toBeNull();
    expect(within(panel).queryByText(REAUTH_HEADING)).toBeNull();
    expect(within(panel).queryByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeNull();
  });

  it("is the amber strip: a warning mark leads the heading, and the whole sentence reads as the board's", async () => {
    mount("alice@corp");
    const panel = await screen.findByTestId("live-approvals");
    expect(panel).toHaveClass("bg-warning-subtle");
    const head = panel.firstElementChild as HTMLElement;
    expect(head.querySelector("svg")).toHaveClass("text-warning");
    expect(head).toHaveTextContent("Corp SSO sign-in needed. Sign in to let this run's model calls through");
  });

  it("its button opens the endpoint's own door", async () => {
    mount("alice@corp");
    await userEvent.click(await screen.findByRole("button", { name: T.BUTTON("Corp SSO") }));
    expect(await screen.findByText(T.DOOR_TITLE("Corp SSO"))).toBeInTheDocument();
  });

  it("another person reads whose sign-in is awaited, and gets no button", async () => {
    mount("bob@corp");
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText(T.APPROVAL_OTHER("alice@corp"))).toBeInTheDocument();
    expect(within(panel).queryByRole("button", { name: /Sign in/ })).toBeNull();
  });

  it("names the provider without the run: the run's provider is not consulted", async () => {
    mount("alice@corp", null);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText(T.APPROVAL_LABEL("Corp SSO"))).toBeInTheDocument();
    expect(within(panel).getByRole("button", { name: T.BUTTON("Corp SSO") })).toBeInTheDocument();
  });

  it("a hold for a second harness's provider B opens B's door while the run's primary is OIDC provider A", async () => {
    listApprovalsMock.mockResolvedValue([
      hold({ requested_scope: { ...hold().requested_scope, provider_id: "uid-b" } }),
    ]);
    mount("alice@corp", "corp-sso", [SSO, OTHER]);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText(T.APPROVAL_HEADING("Other SSO"))).toBeInTheDocument();
    expect(within(panel).queryByText(/Corp SSO/)).toBeNull();
    await userEvent.click(within(panel).getByRole("button", { name: T.BUTTON("Other SSO") }));
    expect(await screen.findByText(T.DOOR_TITLE("Other SSO"))).toBeInTheDocument();
    expect(screen.queryByText(T.DOOR_TITLE("Corp SSO"))).toBeNull();
  });

  it("a hold for OIDC provider B gets B's button when the run's primary is a typed-token provider A", async () => {
    listApprovalsMock.mockResolvedValue([
      hold({ requested_scope: { ...hold().requested_scope, provider_id: "uid-b" } }),
    ]);
    const typed: SetupModelProvider = { ...MODEL_PROVIDERS.anthropicKey, id: "key-a" };
    mount("alice@corp", "key-a", [typed, OTHER]);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByRole("button", { name: T.BUTTON("Other SSO") })).toBeInTheDocument();
    await userEvent.click(within(panel).getByRole("button", { name: T.BUTTON("Other SSO") }));
    expect(await screen.findByText(T.DOOR_TITLE("Other SSO"))).toBeInTheDocument();
  });

  it("a uid with no match in setup status is the generic sign-in-needed, with no door", async () => {
    listApprovalsMock.mockResolvedValue([
      hold({ requested_scope: { ...hold().requested_scope, provider_id: "uid-deleted" } }),
    ]);
    mount("alice@corp", "corp-sso", [SSO, OTHER]);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText(T.APPROVAL_LABEL(T.APPROVAL_UNNAMED))).toBeInTheDocument();
    expect(within(panel).queryByRole("button", { name: /Sign in/ })).toBeNull();
  });

  it("two held requests for different providers read unnamed in the heading", async () => {
    listApprovalsMock.mockResolvedValue([
      hold(),
      hold({ id: "oidc-2", requested_scope: { ...hold().requested_scope, provider_id: "uid-b" } }),
    ]);
    mount("alice@corp", "corp-sso", [SSO, OTHER]);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText(T.APPROVAL_HEADING(T.APPROVAL_UNNAMED))).toBeInTheDocument();
    expect(within(panel).getByRole("button", { name: T.BUTTON("Corp SSO") })).toBeInTheDocument();
    expect(within(panel).getByRole("button", { name: T.BUTTON("Other SSO") })).toBeInTheDocument();
  });
});
