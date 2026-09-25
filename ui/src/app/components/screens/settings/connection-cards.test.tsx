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
import { act, render, screen, waitFor, within } from "@testing-library/react";
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

import { ModelProviderCard, S } from "./connection-cards";
import { OperatorProvider } from "../../wardyn/operator-context";
import { baseStatus } from "../../../lib/test-fixtures";
import type { SetupStatus } from "../../../lib/types";
import { WithDoor } from "../../../../test/door-harness";

const user = userEvent.setup({ pointerEventsCheck: 0 });

// The card's sign-in buttons open the shell's one door (#544); the door reads
// the shell's status, which is the same server answer the card renders.
function model(status: SetupStatus = baseStatus(), onChanged: () => void = vi.fn()) {
  return render(
    <WithDoor status={status}>
      <ModelProviderCard status={status} siteConfig={null} onChanged={onChanged} />
    </WithDoor>,
  );
}

// #337: renders as a MEMBER (operator=false) — every other test in this file
// renders unwrapped, which useOperator()'s fail-open default reads as an
// operator (see operator-context.tsx). Needed to pin what a non-operator
// caller actually sees, not just what an operator sees with `disabled` read
// off the DOM.
function memberModel(status: SetupStatus = baseStatus()) {
  return render(
    <OperatorProvider operator={false}>
      <ModelProviderCard status={status} siteConfig={null} onChanged={vi.fn()} />
    </OperatorProvider>,
  );
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

// R1 (fix-first review pass): the SAME row, but DISABLED. Legal to store
// (validateAgentCredentialSource never looks at Disabled) — but the server's
// login predicate (perUserLoginRow) and model_access scoping (awsSSOScopeFor)
// both treat a disabled row as NOT per_user, so the card must too.
function disabledPerUserStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return baseStatus({
    harnesses: [
      { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: false, mechanism: "bedrock_sso", credential_source: "per_user" },
    ],
    ...overrides,
  });
}

// R3 (fix-first review pass): an EXPLICIT shared row — as opposed to no
// `harnesses` field at all (an older daemon, or before any roster is saved).
// Every "shared row" test below reads this fixture rather than baseStatus()
// so the credential_source and mechanism conjuncts of perUserSso each have a
// test that would fail if either were dropped.
function sharedRowStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return baseStatus({
    harnesses: [
      { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true, mechanism: "bedrock_sso", credential_source: "shared" },
    ],
    ...overrides,
  });
}

// #337: a per_user Bedrock BEARER row — perUserStatus's twin with
// mechanism="bedrock_bearer" instead of "bedrock_sso". A member under THIS
// row is the one whose own stored key their runs actually authenticate with.
function perUserBearerStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return baseStatus({
    harnesses: [{ id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, mechanism: "bedrock_bearer", credential_source: "per_user" }],
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

  // F4-F13 (Appendix A V8): the group had no roving tabindex or arrow keys —
  // every radio was its own Tab stop, and Left/Right did nothing.
  describe("the lane group has roving tabindex and arrow keys", () => {
    // ticket: F4-F13
    it("only the checked radio is a Tab stop; the rest are -1", () => {
      model();
      const radios = screen.getAllByRole("radio");
      const checked = radios.filter((r) => r.getAttribute("aria-checked") === "true");
      const unchecked = radios.filter((r) => r.getAttribute("aria-checked") === "false");
      expect(checked).toHaveLength(1);
      expect(checked[0]).toHaveAttribute("tabIndex", "0");
      for (const r of unchecked) expect(r).toHaveAttribute("tabIndex", "-1");
    });

    it("ArrowRight moves selection and focus to the next lane; ArrowLeft wraps to the last", async () => {
      model();
      const [subscription, apiKey, bedrock] = screen.getAllByRole("radio");
      subscription.focus();
      await user.keyboard("{ArrowRight}");
      expect(apiKey).toHaveFocus();
      expect(apiKey).toHaveAttribute("aria-checked", "true");
      expect(subscription).toHaveAttribute("aria-checked", "false");

      await user.keyboard("{ArrowLeft}");
      expect(subscription).toHaveFocus();
      expect(subscription).toHaveAttribute("aria-checked", "true");

      // Wraps: ArrowLeft off the first item lands on the last.
      await user.keyboard("{ArrowLeft}");
      expect(bedrock).toHaveFocus();
      expect(bedrock).toHaveAttribute("aria-checked", "true");
    });

    // The moved-out body (LaneBody, below the radiogroup — see Lane's own
    // note): selecting a lane by arrow key must open its form exactly as a
    // click would.
    it("selecting Bedrock by arrow key renders its body, not the previous lane's", async () => {
      model();
      const [subscription] = screen.getAllByRole("radio");
      subscription.focus();
      await user.keyboard("{ArrowLeft}"); // wraps to Bedrock
      expect(screen.getByLabelText("Bedrock bearer key")).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Sign in" })).not.toBeInTheDocument();
    });
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

  // #355: SecretLane's "Stored as <name>" caption rendered beside an UNSTORED
  // Save button too — the ternary that also serves the Replace flow's
  // "Replaces <name>" fell to "Stored as" in its else arm, which is exactly
  // the unstored case. A reader took the caption as "already saved". Every
  // SecretLane shares this component (Anthropic/OpenAI keys here, the git
  // PAT/SSH lanes on /providers), so the fix and its pin both live in the
  // shared component, not a Bedrock-only spot (PR #352 review, finding 5).
  describe('#355: "Stored as" only beside an actually-stored secret', () => {
    it("unstored: no \"Stored as\" caption beside Save", async () => {
      model();
      await user.click(screen.getByRole("radio", { name: /API key/ }));
      expect(screen.getByLabelText("Anthropic API key")).toBeInTheDocument();
      expect(screen.queryByText(/Stored as/)).not.toBeInTheDocument();
    });

    it("stored, not editing: \"Stored as <name>\" shows beside Replace/Disconnect", async () => {
      model(baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }));
      await user.click(screen.getByRole("radio", { name: /API key/ }));
      expect(screen.getByRole("button", { name: "Replace" })).toBeInTheDocument();
      expect(screen.getByText(/Stored as/)).toHaveTextContent("anthropic-api-key");
    });

    it("stored, editing (Replace clicked): \"Replaces <name>\", never \"Stored as\"", async () => {
      model(baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }));
      await user.click(screen.getByRole("radio", { name: /API key/ }));
      await user.click(screen.getByRole("button", { name: "Replace" }));
      expect(screen.getByText(/Replaces/)).toHaveTextContent("anthropic-api-key");
      expect(screen.queryByText(/Stored as/)).not.toBeInTheDocument();
    });
  });
});

// F2 (Appendix A #2): an admin signing in from Settings under a per_user row
// was asked to retype the org's AWS access portal URL, and the server
// (harnesscred.go:761) THROWS THE TYPED VALUE AWAY because the row's own
// sso_start_url overrides it. The card now opens the shell's one door (#544),
// which passes startURLManaged on the same condition for every entrance.
describe("ModelProviderCard — the sign-in door under a per_user row", () => {
  // ticket: F2
  it("passes startURLManaged so the dead start-URL prompt never renders", async () => {
    model(perUserStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    await user.click(screen.getByRole("button", { name: /sign in with sso/i }));
    await waitFor(() => expect(loginPaneMock).toHaveBeenCalledWith(expect.objectContaining({ startURLManaged: true })));
  });

  // R3: "no per_user row" covers two distinct cases — no `harnesses` field at
  // all, and an EXPLICIT shared row — kept as separate, honestly-named tests
  // rather than one that only ever exercises the absent-field case.
  it("absent harnesses: the ordinary Settings flow still asks — unspliced control", async () => {
    model();
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    await user.click(screen.getByRole("button", { name: /sign in with sso/i }));
    await waitFor(() => expect(loginPaneMock).toHaveBeenCalledWith(expect.objectContaining({ startURLManaged: false })));
  });

  it("an explicit shared row still asks", async () => {
    model(sharedRowStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    await user.click(screen.getByRole("button", { name: /sign in with sso/i }));
    await waitFor(() => expect(loginPaneMock).toHaveBeenCalledWith(expect.objectContaining({ startURLManaged: false })));
  });

  // R1: a disabled row prompts for the start URL again, exactly like a
  // shared row — the server's own login predicate excludes it too.
  it("a DISABLED per_user row still asks (falls back like a shared row)", async () => {
    model(disabledPerUserStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    await user.click(screen.getByRole("button", { name: /sign in with sso/i }));
    await waitFor(() => expect(loginPaneMock).toHaveBeenCalledWith(expect.objectContaining({ startURLManaged: false })));
  });

  // R3: the `mechanism` conjunct has its own failing case — bedrock_bearer
  // is NOT bedrock_sso, so a per_user credential_source on THAT mechanism is
  // not this lane's per_user SSO at all.
  it("credential_source=per_user on a NON-bedrock_sso mechanism is not per_user SSO", async () => {
    model(
      baseStatus({
        harnesses: [{ id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true, mechanism: "bedrock_bearer", credential_source: "per_user" }],
      }),
    );
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    await user.click(screen.getByRole("button", { name: /sign in with sso/i }));
    await waitFor(() => expect(loginPaneMock).toHaveBeenCalledWith(expect.objectContaining({ startURLManaged: false })));
  });

  // F4: the card says the lane is declared elsewhere and read per-person.
  it("S.BEDROCK_PER_USER_NOTE renders under a per_user row", async () => {
    model(perUserStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByText(S.BEDROCK_PER_USER_NOTE)).toBeInTheDocument();
  });

  it("S.BEDROCK_PER_USER_NOTE is absent when harnesses is absent", async () => {
    model();
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.queryByText(S.BEDROCK_PER_USER_NOTE)).not.toBeInTheDocument();
  });

  it("S.BEDROCK_PER_USER_NOTE is absent for an explicit shared row", async () => {
    model(sharedRowStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.queryByText(S.BEDROCK_PER_USER_NOTE)).not.toBeInTheDocument();
  });

  it("S.BEDROCK_PER_USER_NOTE is absent for a DISABLED per_user row", async () => {
    model(disabledPerUserStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.queryByText(S.BEDROCK_PER_USER_NOTE)).not.toBeInTheDocument();
  });
});

// R9 (fix-first review pass): under per_user, resolveBedrockAuth skips the
// bearer arm outright (Appendix A finding 3) — a stored key still deletes
// fine, but the card must say it is never READ while the row is per_user.
describe("ModelProviderCard — the bearer key is unused under per_user", () => {
  // ticket: R9
  it("renders S.BEDROCK_BEARER_UNUSED_PER_USER under a per_user row", async () => {
    model(perUserStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByText(S.BEDROCK_BEARER_UNUSED_PER_USER)).toBeInTheDocument();
  });

  it("is absent for a shared row", async () => {
    model(sharedRowStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.queryByText(S.BEDROCK_BEARER_UNUSED_PER_USER)).not.toBeInTheDocument();
  });

  it("the bearer SecretLane itself is unaffected — a stored key still shows Disconnect", async () => {
    model(perUserStatus({ secrets: { present: ["bedrock-api-key"], github_app: false } }));
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByText(S.BEDROCK_BEARER_UNUSED_PER_USER)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /disconnect/i })).toBeInTheDocument();
  });
});

// #337: a member on a per_user BEARER row can store and replace their own
// Bedrock bearer key — the console's missing half of #153/#327's server-side
// door. Every "still operator-only" case gets its own test, named for the
// field it pins, rather than one test asserting a count of disabled fields.
describe("ModelProviderCard — #337: a member's own bearer field under a per_user bearer row", () => {
  it("the bearer field is editable for a member on a per_user bearer row", async () => {
    memberModel(perUserBearerStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByLabelText("Bedrock bearer key")).not.toBeDisabled();
  });

  // PR #352 review, finding 1: this fixture used to carry region/model/
  // creds_present alongside bearer_present in a MEMBER render — a shape the
  // server's own redaction (redactSetupStatusForMember, setup.go) never
  // sends, since those three are always dropped for a non-operator. Shaped
  // the way a member's response actually reads now: Ready survives always,
  // BearerPresent survives only under their own per_user bearer row.
  it("stored reflects the member's OWN bearer, not the operator-namespace secrets.present", async () => {
    memberModel(perUserBearerStatus({ bedrock: { ready: true, creds_present: false, bearer_present: true } }));
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByRole("button", { name: /disconnect/i })).toBeInTheDocument();
  });

  // PR #352 review, finding 6: the negative case. secrets.present is an
  // operator-namespace fact, always redacted to empty for a member anyway —
  // this pins that a member with no bearer of their OWN reads not-stored
  // even were that field somehow non-empty, never borrowing anyone else's.
  it("a member with no bearer of their own reads not-stored, even with secrets.present non-empty", async () => {
    memberModel(
      perUserBearerStatus({
        bedrock: { ready: true, creds_present: false, bearer_present: false },
        secrets: { present: ["bedrock-api-key"], github_app: false },
      }),
    );
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.queryByRole("button", { name: /disconnect/i })).not.toBeInTheDocument();
    expect(screen.getByLabelText("Bedrock bearer key")).toBeInTheDocument();
  });

  it("a member on a SHARED row still cannot edit the bearer field", async () => {
    memberModel(sharedRowStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByLabelText("Bedrock bearer key")).toBeDisabled();
  });

  it("a member on a per_user SSO row (not bearer) still cannot edit the bearer field", async () => {
    memberModel(perUserStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByLabelText("Bedrock bearer key")).toBeDisabled();
  });

  it("a DISABLED per_user bearer row still cannot edit the field", async () => {
    memberModel(
      baseStatus({
        harnesses: [
          { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: false, mechanism: "bedrock_bearer", credential_source: "per_user" },
        ],
      }),
    );
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByLabelText("Bedrock bearer key")).toBeDisabled();
  });

  it("a member on a per_user bearer row still cannot edit the Anthropic API key field", async () => {
    memberModel(perUserBearerStatus());
    await user.click(screen.getByRole("radio", { name: /API key/ }));
    expect(screen.getByLabelText("Anthropic API key")).toBeDisabled();
  });

  it("a member on a per_user bearer row still cannot edit the OpenAI API key field", async () => {
    memberModel(perUserBearerStatus());
    await user.click(screen.getByRole("radio", { name: /API key/ }));
    expect(screen.getByLabelText("OpenAI API key")).toBeDisabled();
  });

  it("a member on a per_user bearer row still cannot sign in to the Claude subscription lane", () => {
    memberModel(perUserBearerStatus());
    expect(screen.getByRole("button", { name: /^sign in$/i })).toBeDisabled();
  });

  it("an operator can still edit the bearer field under a per_user bearer row", async () => {
    model(perUserBearerStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByLabelText("Bedrock bearer key")).not.toBeDisabled();
  });

  // PR #352 review, finding 2 (regression): an operator's PUT lands in the
  // "" namespace (runs_policy.go) even under a per_user bearer row —
  // DIFFERENT from bearer_present's per_user-scoped read (the roster owner's
  // own subject namespace, runs_bedrock_probe.go). Reading bearer_present for
  // an operator here showed their own just-saved key as unstored; `present`
  // is the one that matches what their Save actually wrote.
  it("an operator's own stored bearer shows Replace/Disconnect under a per_user bearer row", async () => {
    model(perUserBearerStatus({ secrets: { present: ["bedrock-api-key"], github_app: false } }));
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByRole("button", { name: /disconnect/i })).toBeInTheDocument();
  });

  it("an operator does not read bearer_present under a per_user bearer row", async () => {
    model(perUserBearerStatus({ bedrock: { ready: true, creds_present: false, bearer_present: true } }));
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.queryByRole("button", { name: /disconnect/i })).not.toBeInTheDocument();
  });
});

// PR #352 review, findings 3 + 4: the field's own explanation, instead of
// (finding 3) a card-level "Requires the admin role." sitting directly above
// a field the member CAN edit, or (finding 4) no explanation at all for why
// it's disabled on a shared row.
describe("ModelProviderCard — PR #352 review: the bearer field explains itself", () => {
  it("a member on a per_user bearer row sees the own-key note, and not the admin hint, on the Bedrock lane", async () => {
    memberModel(perUserBearerStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByText(S.BEDROCK_BEARER_OWN_NOTE)).toBeInTheDocument();
    expect(screen.queryByText("Requires the admin role.")).not.toBeInTheDocument();
  });

  it("that same member still sees the admin hint on the API key lane", async () => {
    memberModel(perUserBearerStatus());
    await user.click(screen.getByRole("radio", { name: /API key/ }));
    expect(screen.getByText("Requires the admin role.")).toBeInTheDocument();
  });

  it("a member on a shared row sees the shared reason, not the own-key note", async () => {
    memberModel(sharedRowStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByText(S.BEDROCK_BEARER_SHARED_REASON)).toBeInTheDocument();
    expect(screen.queryByText(S.BEDROCK_BEARER_OWN_NOTE)).not.toBeInTheDocument();
  });

  it("a member on a per_user SSO row sees neither — BEDROCK_BEARER_UNUSED_PER_USER already explains it", async () => {
    memberModel(perUserStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.queryByText(S.BEDROCK_BEARER_SHARED_REASON)).not.toBeInTheDocument();
    expect(screen.queryByText(S.BEDROCK_BEARER_OWN_NOTE)).not.toBeInTheDocument();
    expect(screen.getByText(S.BEDROCK_BEARER_UNUSED_PER_USER)).toBeInTheDocument();
  });

  it("an operator on a per_user bearer row sees the own-key note too", async () => {
    model(perUserBearerStatus());
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByText(S.BEDROCK_BEARER_OWN_NOTE)).toBeInTheDocument();
  });
});

// F5 (Appendix A #5): the Connected badge was keyed on the deployment having
// A Bedrock lane at all (`!!bedrockRow`), never the CALLING principal's own
// model_access — so an admin's browser badged "Connected" from a shared
// admin-token read that can never itself hold an AWS session, and (the sharp
// edge this pins) a member whose OWN sign-in has lapsed still saw green.
describe("ModelProviderCard — the badge follows the caller's own model_access", () => {
  // ticket: F5
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

  // U-02 (blind review lens-U, Appendix A #5's own closing paragraph):
  // not_applicable is the shared admin-token principal's own answer — no
  // per-caller session to grade, and no per-caller answer to substitute
  // either. Borrowing the deployment-wide fact painted a green "Connected"
  // badge over an absent credential for exactly the principal the finding
  // was written about. This card now renders it honestly: NOT connected,
  // with a neutral detail explaining where the lane actually lives.
  it("not_applicable: NOT connected — never borrows the deployment-wide badge", () => {
    model(perUserStatus({ ...bedrockConfigured, model_access: { state: "not_applicable" } }));
    expect(within(screen.getByRole("radio", { name: /AWS Bedrock/ })).queryByText("Connected")).not.toBeInTheDocument();
    expect(screen.getByText(S.BEDROCK_PER_USER_MECHANISM)).toBeInTheDocument();
  });

  // R3: "shared" covers two distinct cases — absent `harnesses` and an
  // EXPLICIT shared row — both pinned rather than only the absent-field one.
  it("absent harnesses ignores model_access entirely", () => {
    model(baseStatus({ ...bedrockConfigured, model_access: { state: "expired_signin" } }));
    expect(within(screen.getByRole("radio", { name: /AWS Bedrock/ })).getByText("Connected")).toBeInTheDocument();
  });

  it("an explicit shared row also ignores model_access entirely", () => {
    model(sharedRowStatus({ ...bedrockConfigured, model_access: { state: "expired_signin" } }));
    expect(within(screen.getByRole("radio", { name: /AWS Bedrock/ })).getByText("Connected")).toBeInTheDocument();
  });

  // R1: a DISABLED per_user row must NOT read the caller's model_access — if
  // the `enabled` conjunct were dropped, this would wrongly read NOT
  // Connected (perUserLive is false for expired_signin) instead of falling
  // back to the deployment-wide fact the way a shared row does.
  it("a DISABLED per_user row falls back to the deployment-wide badge, not the caller's model_access", () => {
    model(disabledPerUserStatus({ ...bedrockConfigured, model_access: { state: "expired_signin" } }));
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

// GitHostCard was retired in 0.7.2 (workspace-providers-prompt.md §2.1, Q4):
// git credential lanes render in a provider row on /providers
// (screens/providers/git-tab.tsx), which has its own test coverage
// (git-tab.test.tsx) for per-host secret naming, the write-only "Connected"
// summary + Replace, the unset-lane input, and Disconnect. Lane/SecretLane/
// HostSummary are the shared shell the Git tab reuses rather than re-typing;
// tsc already enforces these are function components at every import site, so
// no runtime smoke test is needed here — that would be a vacuous re-check of
// a static type. A describe with no it() fails vitest ("No test found in
// suite"), so the empty wrapper goes with it.

// U2-01 (blind round 2, lens-U2): the NON-per_user arm of the same badge.
// `!!bedrockRow` is true as soon as deriveIntegrations sees a Bedrock row at
// all, and integrations.ts builds that row from `region || model ||
// creds_present || aws_mount || bearer_present` — so region + model with NO
// credential of any kind painted a green "Connected" over an absent
// credential. That is not a hypothetical: scripts/e2e-backend.sh configures
// exactly those two and nothing else. The honest signal is the row's
// `bedrockLane`, which activeBedrockLane() leaves undefined until a
// credential lane is actually active.
describe("ModelProviderCard — Connected needs an active credential lane, not region+model", () => {
  // ticket: U2-01
  const bedrockLane = () => within(screen.getByRole("radio", { name: /AWS Bedrock/ }));

  it("region + model with NO credential of any kind: NOT Connected", () => {
    model(baseStatus({ bedrock: { region: "us-east-1", model: "anthropic.claude", creds_present: false } }));
    expect(bedrockLane().queryByText("Connected")).not.toBeInTheDocument();
  });

  it("a stored bearer key: Connected", () => {
    model(baseStatus({ bedrock: { region: "us-east-1", model: "anthropic.claude", creds_present: false, bearer_present: true } }));
    expect(bedrockLane().getByText("Connected")).toBeInTheDocument();
  });

  it("a host ~/.aws mount: Connected", () => {
    model(baseStatus({ bedrock: { region: "us-east-1", model: "anthropic.claude", creds_present: false, aws_mount: true } }));
    expect(bedrockLane().getByText("Connected")).toBeInTheDocument();
  });

  it("static access keys: Connected", () => {
    model(baseStatus({ bedrock: { region: "us-east-1", model: "anthropic.claude", creds_present: true } }));
    expect(bedrockLane().getByText("Connected")).toBeInTheDocument();
  });

  it("a member's redacted status ({ready} only — region/model/lanes withheld): Connected", () => {
    // ticket: B7-F6 (rider)
    model(baseStatus({ bedrock: { ready: true, creds_present: false } }));
    expect(bedrockLane().getByText("Connected")).toBeInTheDocument();
  });

  it("ready:false with region+model and no lane stays NOT Connected (the negative control, spelled out)", () => {
    // ticket: U2-01
    model(baseStatus({ bedrock: { ready: false, region: "us-east-1", model: "anthropic.claude", creds_present: false } }));
    expect(bedrockLane().queryByText("Connected")).not.toBeInTheDocument();
  });

  it("an explicit shared row with region+model only is ALSO not Connected", () => {
    model(sharedRowStatus({ bedrock: { region: "us-east-1", model: "anthropic.claude", creds_present: false } }));
    expect(bedrockLane().queryByText("Connected")).not.toBeInTheDocument();
  });

  it("a DISABLED per_user row with region+model only is ALSO not Connected", () => {
    model(disabledPerUserStatus({ bedrock: { region: "us-east-1", model: "anthropic.claude", creds_present: false } }));
    expect(bedrockLane().queryByText("Connected")).not.toBeInTheDocument();
  });
});

// U2-03 (blind round 2, lens-U2): `not_applicable` means the caller is a
// MECHANISM, not a person — "no sign-in it could complete"
// (internal/api/modelaccess.go). Leaving an enabled "Sign in with SSO" and an
// imperative beside the honest badge offers a door that answers 422
// (harnessLoginMechanismPrincipalRefusal). agents-tab.tsx:253 already drops
// its whole model-access block for this state; the card keeps the sentence
// (the badge needs a reason) but drops the imperative and the door.
describe("ModelProviderCard — not_applicable keeps no door it cannot open", () => {
  // ticket: U2-03
  const notApplicable = { bedrock: { region: "us-east-1", model: "anthropic.claude", creds_present: true }, model_access: { state: "not_applicable" } };

  it("renders the mechanism sentence with no imperative", async () => {
    model(perUserStatus(notApplicable));
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByText(S.BEDROCK_PER_USER_MECHANISM)).toBeInTheDocument();
    expect(S.BEDROCK_PER_USER_MECHANISM).not.toMatch(/sign in to see/i);
  });

  it("drops the Sign in with SSO button entirely", async () => {
    model(perUserStatus(notApplicable));
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.queryByRole("button", { name: "Sign in with SSO" })).not.toBeInTheDocument();
  });

  it("negative control: an actionable per_user state still offers the door", async () => {
    model(perUserStatus({ ...notApplicable, model_access: { state: "expired_signin" } }));
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByRole("button", { name: "Sign in with SSO" })).toBeInTheDocument();
  });
});

// #544: the card mounts no pane of its own — each lane's button opens the
// shell's one door for its own sign-in.
describe("ModelProviderCard — both sign-in buttons open the one door", () => {
  it("Claude subscription's Sign in opens the Claude door", async () => {
    model();
    await user.click(screen.getByRole("button", { name: /^sign in$/i }));
    expect(await screen.findByRole("dialog", { name: "Sign in to Claude" })).toBeInTheDocument();
    await waitFor(() => expect(loginPaneMock).toHaveBeenCalledWith(expect.objectContaining({ provider: "anthropic" })));
  });

  it("AWS Bedrock's Sign in with SSO opens the AWS door", async () => {
    model();
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    await user.click(screen.getByRole("button", { name: /sign in with sso/i }));
    expect(await screen.findByRole("dialog", { name: "Sign in to AWS" })).toBeInTheDocument();
    await waitFor(() => expect(loginPaneMock).toHaveBeenCalledWith(expect.objectContaining({ provider: "aws" })));
  });
});

// U2-09 (blind round 2, lens-U2): CAPTURE_CHECK_UNREACHABLE leaves the pane
// on the error phase with the capture possibly landed — onDone never fires,
// so the parent never refreshes and the card keeps reading not-connected
// until a manual reload. Cancel makes no claim about the capture either way;
// it just costs one GET.
describe("ModelProviderCard — dismissing the login dialog re-reads status", () => {
  // ticket: U2-09
  async function openLogin(onChanged: () => void) {
    model(baseStatus(), onChanged);
    await user.click(screen.getByRole("button", { name: /^sign in$/i }));
    await screen.findByRole("dialog");
    await waitFor(() => expect(loginPaneMock).toHaveBeenCalled());
    expect(onChanged).not.toHaveBeenCalled();
  }

  it("the pane's own Cancel calls onChanged, like Done already does", async () => {
    const onChanged = vi.fn();
    await openLogin(onChanged);
    const props = loginPaneMock.mock.calls.at(-1)![0] as { onCancel: () => void };
    await act(async () => props.onCancel());
    expect(onChanged).toHaveBeenCalled();
  });

  // Escape / overlay click is the SAME dismissal — one closer behind both, so
  // neither door can drift back to leaving the card stale.
  it("Escape does too", async () => {
    const onChanged = vi.fn();
    await openLogin(onChanged);
    await user.keyboard("{Escape}");
    expect(onChanged).toHaveBeenCalled();
  });
});
