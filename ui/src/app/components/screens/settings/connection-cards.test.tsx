/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The two shared connection cards. What matters here is that a lane's
// "Connected" state comes from the SAME derivation readiness.ts uses
// (deriveIntegrations over the real SetupStatus), not a bespoke re-read of raw
// fields — a card that disagrees with the app-shell chip is the exact class of
// bug the old two-model /integrations page kept producing.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const setSecretMock = vi.fn();
const deleteSecretMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: {
    setSecret: (...a: unknown[]) => setSecretMock(...a),
    deleteSecret: (...a: unknown[]) => deleteSecretMock(...a),
  },
}));
// HarnessLoginPane drives a real PTY through AttachTerminal (xterm), which does
// not render in jsdom — the cards only own the button that opens it, and the
// PROP the card hands it is what F2's test below pins (loginPaneMock records
// every render's props).
const loginPaneMock = vi.fn();
vi.mock("./harness-login-pane", () => ({
  HarnessLoginPane: (props: { startURLManaged?: boolean }) => {
    loginPaneMock(props);
    return <div data-testid="login-pane" />;
  },
}));

import { HostSummary, Lane, ModelProviderCard, S, SecretLane } from "./connection-cards";
import { baseStatus } from "../../../lib/test-fixtures";
import type { SetupStatus } from "../../../lib/types";

const user = userEvent.setup({ pointerEventsCheck: 0 });

function model(status: SetupStatus = baseStatus()) {
  return render(<ModelProviderCard status={status} siteConfig={null} onChanged={vi.fn()} />);
}

beforeEach(() => {
  setSecretMock.mockReset().mockResolvedValue(undefined);
  deleteSecretMock.mockReset().mockResolvedValue(undefined);
  loginPaneMock.mockReset();
});

// A per_user Bedrock row (harnesses[claude-code].mechanism="bedrock_sso",
// credential_source="per_user") — the roster shape the Agents tab writes.
function perUserStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return baseStatus({
    harnesses: [{ id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, mechanism: "bedrock_sso", credential_source: "per_user" }],
    ...overrides,
  });
}

describe("ModelProviderCard", () => {
  it("offers exactly the three lanes the mock settled on — no Azure, no catalog", () => {
    model();
    const group = screen.getByRole("radiogroup", { name: S.MODEL_TITLE });
    expect(screen.getAllByRole("radio").length).toBe(3);
    expect(group).toBeInTheDocument();
    expect(screen.queryByText(/Azure/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /add integration/i })).not.toBeInTheDocument();
  });

  // A shared subscription credential is one person's; on a multi-user deployment
  // injecting it into other people's runs breaches the harness vendor's per-user
  // authentication terms, and the OPERATOR is the one in breach. The daemon
  // refuses it there, so the console must not offer a sign-in that cannot work —
  // and must say why, because a missing affordance reads as "nobody connected one
  // yet" and a greyed-out one reads as "you lack permission".
  it("shared subscription blocked: explains instead of offering a sign-in", () => {
    const st = baseStatus();
    st.auth.shared_subscription_allowed = false;
    st.auth.shared_subscription_reason = "OIDC/SSO is configured, which declares that more than one human uses this deployment";
    model(st);
    expect(screen.queryByRole("button", { name: /sign in$/i })).not.toBeInTheDocument();
    expect(screen.getByText(/Unavailable in this deployment/i)).toBeInTheDocument();
    expect(screen.getByText(/more than one human/i)).toBeInTheDocument();
  });

  it("shared subscription allowed: the sign-in is offered", () => {
    const st = baseStatus();
    st.auth.shared_subscription_allowed = true;
    model(st);
    expect(screen.getByRole("button", { name: /sign in$/i })).toBeInTheDocument();
  });

  // An older daemon omits the field entirely. Absent must read as ALLOWED so this
  // console keeps working against one — the daemon is the enforcement point.
  it("field absent (older daemon): the sign-in is still offered", () => {
    const st = baseStatus();
    delete st.auth.shared_subscription_allowed;
    model(st);
    expect(screen.getByRole("button", { name: /sign in$/i })).toBeInTheDocument();
  });

  it("nothing stored: no lane reads Connected", () => {
    model();
    expect(screen.queryByText("Connected")).not.toBeInTheDocument();
  });

  // The honesty invariant readiness.ts shares: a stored key IS a real path, and
  // the card must say so without a probe (Wardyn never dials the provider).
  it("an anthropic key secret makes the API key lane read Connected", () => {
    model(baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }));
    expect(screen.getByText("Connected")).toBeInTheDocument();
  });

  it("a captured managed subscription reads Connected and offers Disconnect", () => {
    model(baseStatus({ harness: [{ provider: "anthropic", captured: true }] }));
    expect(screen.getByText("Connected")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /disconnect/i })).toBeInTheDocument();
  });

  // A host-CLI login lives in the operator's own ~/.claude — Wardyn can read it
  // but cannot revoke it, so offering a Disconnect would be a button that lies.
  it("a HOST-CLI subscription reads Connected but offers no Disconnect", () => {
    model(
      baseStatus({ providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }] }),
    );
    expect(screen.getByText("Connected")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /disconnect/i })).not.toBeInTheDocument();
  });

  it("saving an API key writes the conventional secret name", async () => {
    model();
    await user.click(screen.getByRole("radio", { name: /API key/ }));
    await user.type(screen.getByLabelText("Anthropic API key"), "sk-ant-test");
    await user.click(screen.getAllByRole("button", { name: /^save$/i })[0]);
    expect(setSecretMock).toHaveBeenCalledWith("anthropic-api-key", "sk-ant-test");
  });

  // Bedrock's region/model are boot-time daemon config (runs_bedrock.go), so the
  // card must state where they come from rather than render an input the server
  // would ignore.
  it("Bedrock names its config source instead of offering region/model inputs", async () => {
    model();
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByText(new RegExp(S.BEDROCK_CONFIG_NOTE.slice(0, 40)))).toBeInTheDocument();
    expect(screen.queryByLabelText(/^region$/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/^model$/i)).not.toBeInTheDocument();
  });
});

// F2 (Appendix A #2): three call sites open HarnessLoginPane; two pass
// startURLManaged, this card didn't — an admin signing in from Settings under
// a per_user row was asked to retype the org's AWS access portal URL, and the
// server (harnesscred.go:761) THROWS THE TYPED VALUE AWAY because the row's
// own sso_start_url overrides it. Fix = pass the prop on the same condition
// the Agents tab already uses.
describe("ModelProviderCard — F2: the sign-in door under a per_user row", () => {
  it("passes startURLManaged so the dead start-URL prompt never renders", async () => {
    model(perUserStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    await user.click(screen.getByRole("button", { name: /sign in with sso/i }));
    expect(loginPaneMock).toHaveBeenCalledWith(expect.objectContaining({ startURLManaged: true }));
  });

  it("the ordinary Settings flow (no per_user row) still asks — unspliced control", async () => {
    model();
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    await user.click(screen.getByRole("button", { name: /sign in with sso/i }));
    expect(loginPaneMock).toHaveBeenCalledWith(expect.objectContaining({ startURLManaged: false }));
  });

  // F4: the card says the lane is declared elsewhere and read per-person.
  it("S.BEDROCK_PER_USER_NOTE renders under a per_user row", async () => {
    model(perUserStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByText(S.BEDROCK_PER_USER_NOTE)).toBeInTheDocument();
  });

  it("S.BEDROCK_PER_USER_NOTE is absent for a shared (non-per_user) row", async () => {
    model();
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.queryByText(S.BEDROCK_PER_USER_NOTE)).not.toBeInTheDocument();
  });
});

// F5 (Appendix A #5): the Connected badge was keyed on the deployment having
// A Bedrock lane at all (`!!bedrockRow`), never the CALLING principal's own
// model_access — so an admin's browser badged "Connected" from a shared
// admin-token read that can never itself hold an AWS session, and (the sharp
// edge this pins) a member whose OWN sign-in has lapsed still saw green.
describe("ModelProviderCard — F5: the badge follows the caller's own model_access", () => {
  const bedrockConfigured = { bedrock: { region: "us-east-1", model: "anthropic.claude", creds_present: true } };

  it("live: Connected", () => {
    model(perUserStatus({ ...bedrockConfigured, model_access: { state: "live" } }));
    expect(within(screen.getByRole("radio", { name: /AWS Bedrock/ })).getByText("Connected")).toBeInTheDocument();
  });

  it("expiring: still Connected (the session still signs)", () => {
    model(perUserStatus({ ...bedrockConfigured, model_access: { state: "expiring" } }));
    expect(within(screen.getByRole("radio", { name: /AWS Bedrock/ })).getByText("Connected")).toBeInTheDocument();
  });

  it("expired_signin: NOT Connected, even though the deployment has a Bedrock lane", () => {
    model(perUserStatus({ ...bedrockConfigured, model_access: { state: "expired_signin" } }));
    expect(within(screen.getByRole("radio", { name: /AWS Bedrock/ })).queryByText("Connected")).not.toBeInTheDocument();
  });

  it("not_configured: NOT Connected", () => {
    model(perUserStatus({ ...bedrockConfigured, model_access: { state: "not_configured" } }));
    expect(within(screen.getByRole("radio", { name: /AWS Bedrock/ })).queryByText("Connected")).not.toBeInTheDocument();
  });

  // not_applicable: the caller IS the shared admin token, which owns no
  // per-person session to grade — falls back to the deployment-wide answer
  // (there is no per-caller answer to substitute).
  it("not_applicable: falls back to the deployment-wide badge", () => {
    model(perUserStatus({ ...bedrockConfigured, model_access: { state: "not_applicable" } }));
    expect(within(screen.getByRole("radio", { name: /AWS Bedrock/ })).getByText("Connected")).toBeInTheDocument();
  });

  // A shared (non-per_user) row is unaffected — the deployment-wide badge is
  // still the right answer when there is no per-person credential to grade.
  it("a shared row ignores model_access entirely", () => {
    model(baseStatus({ ...bedrockConfigured, model_access: { state: "expired_signin" } }));
    expect(within(screen.getByRole("radio", { name: /AWS Bedrock/ })).getByText("Connected")).toBeInTheDocument();
  });
});

// The login dialog hosts a live PTY, which makes its geometry load-bearing in
// ways no other dialog's is. Both rules below were BROKEN in the first cut and
// are pinned here because neither is visible in a unit render — only in a
// browser, at a real viewport, with a real terminal in the box.
describe("ModelProviderCard — the login dialog's geometry", () => {
  it("clears BOTH translate and transform inline — they are separate properties", async () => {
    model();
    await user.click(screen.getByRole("button", { name: /^sign in$/i }));
    const dialog = await screen.findByRole("dialog");
    // In Tailwind v4 `translate-x-[-50%]` emits the INDIVIDUAL `translate`
    // property, which `transform: none` does not reset — measured in a browser:
    // computed transform "none" alongside translate "-50% -50%", which shifted
    // this dialog half its own size off-centre (left 122px against a computed
    // margin-left of 698px). Both are cleared, and inline so no utility can
    // outrank them. Centring then comes from inset-0 + m-auto + h-fit.
    expect(dialog.style.translate).toBe("none");
    expect(dialog.style.transform).toBe("none");
    expect(dialog.className).toContain("m-auto");
    expect(dialog.className).toContain("h-fit");
  });

  it("sets its width inline, where no utility-order race can reach it", async () => {
    model();
    await user.click(screen.getByRole("button", { name: /^sign in$/i }));
    const dialog = await screen.findByRole("dialog");
    // DialogContent ships `w-full` + `sm:max-w-lg`; a competing max-w-* class is
    // the same specificity, so the winner is decided by utility order in the
    // compiled stylesheet — and sm:max-w-lg measurably beat both max-w-3xl and
    // sm:max-w-[72rem]. An inline style beats every class.
    expect(dialog.style.maxWidth).toBe("min(96vw, 72rem)");
  });

  it("caps its height and scrolls, so a tall terminal cannot push the footer off-screen", async () => {
    model();
    await user.click(screen.getByRole("button", { name: /^sign in$/i }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog.className).toContain("max-h-[92vh]");
    expect(dialog.className).toContain("overflow-y-auto");
  });
});

// GitHostCard retired in 0.7.2 (workspace-providers-prompt.md §2.1, Q4): the
// git credential lanes it rendered moved into a provider row on /providers
// (screens/providers/git-tab.tsx), which has its own test coverage
// (git-tab.test.tsx) for the lane behaviors this block used to pin here —
// per-host secret naming, the write-only "Connected" summary + Replace, the
// unset-lane input, and Disconnect. What stays HERE is the export smoke test:
// Lane/SecretLane/HostSummary are the shared shell the Git tab reuses rather
// than re-typing.
describe("Lane / SecretLane / HostSummary — exported for the Workspace Providers Git tab", () => {
  it("are exported function components", () => {
    expect(typeof Lane).toBe("function");
    expect(typeof SecretLane).toBe("function");
    expect(typeof HostSummary).toBe("function");
  });
});
