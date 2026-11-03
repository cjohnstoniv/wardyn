/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The /approvals card for a held run's sign-in to an endpoint that signs people in: provider-named once the run's
// own provider is read, the owner's door opens that provider's sign-in, and another person reads whose it is.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest, MeCapabilities, SetupModelProvider } from "../../lib/types";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { OIDC_SIGN_IN as T } from "../wardyn/copy/oidc-signin";
import { OIDC_HOLD_LANE, REAUTH_ROW } from "../wardyn/model-access-copy";
import { providerStatus } from "../../lib/test-fixtures";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
vi.mock("./settings/harness-login-pane", () => ({ HarnessLoginPane: () => <div data-testid="fake-pane" /> }));

const SCOPE = { lane: OIDC_HOLD_LANE, mechanism: "ado_entra", reason: "x", owner: "alice@corp", provider_id: "uid-1" };
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: async (state: string) =>
      state === "PENDING"
        ? [{ id: "apr_1", run_id: "run_1", kind: "credential_reauth", requested_scope: SCOPE, state: "PENDING", requested_at: new Date().toISOString() } satisfies ApprovalRequest]
        : [],
    approve: vi.fn(),
    deny: vi.fn(),
  },
}));
const caps: MeCapabilities = { grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false, kinds_version: 1 };
vi.mock("../../lib/api/permissions", () => ({ permissions: { getMyCapabilities: () => Promise.resolve(caps) } }));
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: () =>
      Promise.resolve({
        id: "run_1", agent: "claude-code", task: "t", confinement_class: "CC2", state: "RUNNING", created_by: "alice@corp",
        model_provider_id: "corp-sso", model_provider_name: "Corp SSO",
      }),
  },
}));

import { ApprovalsScreen } from "./approvals";
import { OperatorProvider } from "../wardyn/operator-context";

const SSO: SetupModelProvider = {
  id: "corp-sso", name: "Corp SSO", kind: "custom_endpoint", harnesses: ["claude-code"], host: "llm.contoso.example", sign_in: "oidc",
};

function mount(principal: string) {
  render(
    <OperatorProvider operator={false} securityOperator={false} principal={principal}>
      <MemoryRouter initialEntries={["/approvals"]}>
        <ModelAccessProvider status={providerStatus([{ provider: SSO, state: "expired_signin" }])} onRefresh={() => {}}>
          <ApprovalsScreen />
        </ModelAccessProvider>
      </MemoryRouter>
    </OperatorProvider>,
  );
}

describe("/approvals — a held sign-in to an endpoint", () => {
  it("the owner: a sign-in, not a decision; provider-named chip, what line and button; no AWS wording", async () => {
    mount("alice@corp");
    expect(await screen.findByText(T.APPROVAL_LABEL("Corp SSO"))).toBeInTheDocument();
    expect(screen.getByText(T.APPROVAL_CHIP)).toBeInTheDocument();
    expect(screen.getByText(`${T.APPROVAL_LABEL("Corp SSO")}.`)).toBeInTheDocument();
    expect(screen.getByText(`Model provider · Corp SSO`)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Approve$/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Deny$/ })).toBeNull();
    expect(screen.queryByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: T.BUTTON("Corp SSO") }));
    expect(await screen.findByText(T.DOOR_TITLE("Corp SSO"))).toBeInTheDocument();
  });

  it("another person: whose sign-in it is, and no button", async () => {
    mount("bob@corp");
    expect(await screen.findByText(T.APPROVAL_OTHER("alice@corp"))).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: T.BUTTON("Corp SSO") })).toBeNull();
  });
});
