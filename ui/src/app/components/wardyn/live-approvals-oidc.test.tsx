/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The held-run sign-in row for an endpoint that signs people in. Its request names its provider row by uid, which
// the console cannot read, so the provider is the run's own; the AWS wording and the AWS door are never used for it.
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
import { providerStatus } from "../../lib/test-fixtures";
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
};
const hold = (over: Partial<ApprovalRequest> = {}): ApprovalRequest =>
  makeApproval({
    id: "oidc-1",
    run_id: "r1",
    kind: "credential_reauth",
    requested_scope: { lane: OIDC_HOLD_LANE, mechanism: "ado_entra", reason: "x", owner: "alice@corp", provider_id: "uid-123" },
    state: "PENDING",
    requested_at: new Date().toISOString(),
    ...over,
  });

function mount(principal: string, runProvider: string | null = "corp-sso") {
  const status = providerStatus([{ provider: SSO, state: "expired_signin" }]);
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
  it("reads the lane, and resolves the provider only from the run's own and only when it signs people in", () => {
    const a = reauthAudience(hold(), { operator: false, principal: "alice@corp" });
    expect(a.oidc).toBe(true);
    expect(a.provider).toBe("");
    const status = providerStatus([{ provider: SSO }]);
    expect(oidcHoldProvider(a, status, "corp-sso")?.id).toBe("corp-sso");
    expect(oidcHoldProvider(a, status, undefined)).toBeUndefined();
    expect(oidcHoldProvider(a, status, "someone-else")).toBeUndefined();
    const aws = reauthAudience(hold({ requested_scope: { provider: "bedrock-prod", owner: "alice@corp" } }), { operator: false, principal: "alice@corp" });
    expect(aws.oidc).toBe(false);
    expect(oidcHoldProvider(aws, status, "corp-sso")).toBeUndefined();
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

  it("with the run's provider unknown, offers no door rather than the AWS one", async () => {
    mount("alice@corp", null);
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText(T.APPROVAL_LABEL(T.APPROVAL_UNNAMED))).toBeInTheDocument();
    expect(within(panel).queryByRole("button", { name: /Sign in/ })).toBeNull();
  });
});
