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
import { render, screen } from "@testing-library/react";
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
// not render in jsdom — the cards only own the button that opens it.
vi.mock("./harness-login-pane", () => ({ HarnessLoginPane: () => <div data-testid="login-pane" /> }));

import { ModelProviderCard, GitHostCard, S } from "./connection-cards";
import { baseStatus } from "../../../lib/test-fixtures";
import type { SetupStatus } from "../../../lib/types";

const user = userEvent.setup({ pointerEventsCheck: 0 });

function model(status: SetupStatus = baseStatus()) {
  return render(<ModelProviderCard status={status} siteConfig={null} onChanged={vi.fn()} />);
}

beforeEach(() => {
  setSecretMock.mockReset().mockResolvedValue(undefined);
  deleteSecretMock.mockReset().mockResolvedValue(undefined);
});

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

describe("GitHostCard", () => {
  function git(status: SetupStatus = baseStatus()) {
    return render(<GitHostCard status={status} siteConfig={null} onChanged={vi.fn()} />);
  }

  it("offers the three lanes and defaults to github.com", () => {
    git();
    expect(screen.getByRole("radiogroup", { name: S.GIT_TITLE })).toBeInTheDocument();
    expect(screen.getAllByRole("radio").length).toBe(3);
    expect(screen.getByLabelText("Host")).toHaveValue("github.com");
  });

  // The secret name is per-host, so the card can't pretend there is a single
  // global git credential — retyping the host retargets the write.
  it("the PAT write is named for the host, not a global", async () => {
    git();
    await user.type(screen.getByLabelText("Access token"), "ghp_test");
    await user.click(screen.getByRole("button", { name: /^save$/i }));
    expect(setSecretMock).toHaveBeenCalledWith("git-pat-github-com", "ghp_test");
  });

  // Reported from the live console: a lane that had just been set up read
  // "Connected" and STILL showed an empty key box, which looks exactly like a
  // save that didn't take. The value is write-only, so there is nothing to
  // prefill it with — the honest surface is a summary plus a deliberate Replace.
  it("a stored lane shows no input until Replace is pressed", async () => {
    git(baseStatus({ secrets: { present: ["git-pat-github-com"], github_app: false } }));
    expect(screen.getByText("Connected")).toBeInTheDocument();
    expect(screen.queryByLabelText("Access token")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Host")).not.toBeInTheDocument();
    expect(screen.getByText(/write-only and never read back/i)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /^replace$/i }));
    expect(screen.getByLabelText("Access token")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /save replacement/i })).toBeInTheDocument();

    // Cancel puts it back — an operator who opened it by mistake is not stuck
    // looking at an empty box again.
    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(screen.queryByLabelText("Access token")).not.toBeInTheDocument();
  });

  it("an UNSET lane shows its input immediately — nothing to summarise yet", () => {
    git();
    expect(screen.getByLabelText("Access token")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^save$/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^replace$/i })).not.toBeInTheDocument();
  });

  it("a stored PAT reads Connected and can be disconnected", async () => {
    git(baseStatus({ secrets: { present: ["git-pat-github-com"], github_app: false } }));
    expect(screen.getByText("Connected")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /^disconnect$/i }));
    expect(deleteSecretMock).toHaveBeenCalledWith("git-pat-github-com");
  });
});
