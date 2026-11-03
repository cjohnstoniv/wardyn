/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The /approvals card for a held run's sign-in to an endpoint that signs people in: provider-named from the hold's
// own uid (never the run's provider), the owner's door opens that provider's sign-in, and another person reads whose it is.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest, MeCapabilities, SetupModelProvider } from "../../lib/types";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { OIDC_SIGN_IN as T } from "../wardyn/copy/oidc-signin";
import { OIDC_HOLD_LANE, REAUTH_ROW } from "../wardyn/model-access-copy";
import { MODEL_PROVIDERS, providerStatus } from "../../lib/test-fixtures";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
vi.mock("./settings/harness-login-pane", () => ({ HarnessLoginPane: () => <div data-testid="fake-pane" /> }));

let holdUID = "uid-a";
let runProvider = "corp-sso";
const scope = () => ({ lane: OIDC_HOLD_LANE, mechanism: "ado_entra", reason: "x", owner: "alice@corp", provider_id: holdUID });
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: async (state: string) =>
      state === "PENDING"
        ? [{ id: "apr_1", run_id: "run_1", kind: "credential_reauth", requested_scope: scope(), state: "PENDING", requested_at: new Date().toISOString() } satisfies ApprovalRequest]
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
        model_provider_id: runProvider,
      }),
  },
}));

import { ApprovalsScreen } from "./approvals";
import { OperatorProvider } from "../wardyn/operator-context";

const SSO: SetupModelProvider = {
  id: "corp-sso", name: "Corp SSO", kind: "custom_endpoint", harnesses: ["claude-code"], host: "llm.contoso.example", sign_in: "oidc", uid: "uid-a",
};
const OTHER: SetupModelProvider = { ...SSO, id: "other-sso", name: "Other SSO", host: "llm.other.example", uid: "uid-b" };

function mount(principal: string, rows: SetupModelProvider[] = [SSO]) {
  render(
    <OperatorProvider operator={false} securityOperator={false} principal={principal}>
      <MemoryRouter initialEntries={["/approvals"]}>
        <ModelAccessProvider status={providerStatus(rows.map((provider) => ({ provider, state: "expired_signin" })))} onRefresh={() => {}}>
          <ApprovalsScreen />
        </ModelAccessProvider>
      </MemoryRouter>
    </OperatorProvider>,
  );
}

describe("/approvals — a held sign-in to an endpoint", () => {
  beforeEach(() => {
    holdUID = "uid-a";
    runProvider = "corp-sso";
  });

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

  it("a hold for a second harness's provider B opens B's door while the run's primary is OIDC provider A", async () => {
    holdUID = "uid-b";
    mount("alice@corp", [SSO, OTHER]);
    expect(await screen.findByText(T.APPROVAL_LABEL("Other SSO"))).toBeInTheDocument();
    expect(screen.queryByText(/Corp SSO/)).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: T.BUTTON("Other SSO") }));
    expect(await screen.findByText(T.DOOR_TITLE("Other SSO"))).toBeInTheDocument();
  });

  it("a hold for OIDC provider B gets B's button when the run's primary is a typed-token provider A", async () => {
    holdUID = "uid-b";
    runProvider = "key-a";
    mount("alice@corp", [{ ...MODEL_PROVIDERS.anthropicKey, id: "key-a" }, OTHER]);
    await userEvent.click(await screen.findByRole("button", { name: T.BUTTON("Other SSO") }));
    expect(await screen.findByText(T.DOOR_TITLE("Other SSO"))).toBeInTheDocument();
  });

  it("a uid with no match is the generic sign-in-needed with no door, even when the run's provider signs in", async () => {
    holdUID = "uid-deleted";
    mount("alice@corp", [SSO, OTHER]);
    expect(await screen.findByText(T.APPROVAL_LABEL(T.APPROVAL_UNNAMED))).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Sign in/ })).toBeNull();
  });
});
