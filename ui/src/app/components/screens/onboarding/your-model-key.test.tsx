/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";

const setSecretMock = vi.fn();
const deleteSecretMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: {
    setSecret: (...a: unknown[]) => setSecretMock(...a),
    deleteSecret: (...a: unknown[]) => deleteSecretMock(...a),
  },
}));

import { YourModelKey } from "./your-model-key";
import { YOUR_MODEL_KEY as T } from "../../wardyn/copy";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import type { SetupHarnessTool } from "../../../lib/types";

// Appendix A finding 2 — a per_user roster row (the wire shape a real
// bedrock_sso lane sends: modelKeyProvider picks the first enabled row in
// T.BY_AGENT order and this test's id matches the default claude-code key).
const perUserHarness: SetupHarnessTool[] = [
  { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true, credential_source: "per_user" },
];

describe("YourModelKey", () => {
  beforeEach(() => {
    setSecretMock.mockReset().mockResolvedValue(undefined);
    deleteSecretMock.mockReset().mockResolvedValue(undefined);
  });

  it("empty state: no own key, admin hasn't provided one — shows the bring-your-own form", () => {
    render(<YourModelKey llmReady={false} mine={[]} variant="default" onChanged={() => {}} />);

    expect(screen.getByText(/Bring your own key/)).toBeInTheDocument();
    expect(screen.getByText("anthropic-api-key")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("sk-ant-…")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save key" })).toBeDisabled();
  });

  it("empty state while mine is still loading (null) — never a premature Done/provided read", () => {
    render(<YourModelKey llmReady={false} mine={null} variant="default" onChanged={() => {}} />);
    expect(screen.getByPlaceholderText("sk-ant-…")).toBeInTheDocument();
  });

  it("set state: own key present — masked value, Rotate/Remove, Done in the header", () => {
    render(<YourModelKey llmReady={false} mine={["anthropic-api-key"]} variant="outline" onChanged={() => {}} />);

    expect(screen.getByText("••••••••••••")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Rotate" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove" })).toBeInTheDocument();
    expect(screen.getByText("Done")).toBeInTheDocument();
    expect(screen.getByText(/Your runs can use this key/)).toBeInTheDocument();
  });

  it("Rotate opens the form; Cancel backs out to the masked value with no save", async () => {
    const user = userEvent.setup();
    render(<YourModelKey llmReady={false} mine={["anthropic-api-key"]} variant="outline" onChanged={() => {}} />);

    await user.click(screen.getByRole("button", { name: "Rotate" }));
    expect(screen.getByPlaceholderText("sk-ant-…")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByPlaceholderText("sk-ant-…")).not.toBeInTheDocument();
    expect(screen.getByText("••••••••••••")).toBeInTheDocument();
    expect(setSecretMock).not.toHaveBeenCalled();
  });

  it("provided state (negative control: modelAccess absent falls back to today's copy): admin covers model access — the collapsed read-only view, revealed by 'Use my own key instead'", async () => {
    const user = userEvent.setup();
    render(<YourModelKey llmReady={true} mine={[]} modelAccess={undefined} variant="default" onChanged={() => {}} />);

    expect(screen.getByText("Provided by your admin")).toBeInTheDocument();
    expect(screen.getByText("Model access is already configured for you.")).toBeInTheDocument();
    expect(screen.queryByPlaceholderText("sk-ant-…")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Use my own key instead" }));
    expect(screen.getByPlaceholderText("sk-ant-…")).toBeInTheDocument();
  });

  // Appendix A finding 2 — five per-principal states through the CARD. Each
  // is a per_user roster row except shared_expired, which only exists in the
  // shared/none bucket (a per_user row's own "other" state renders as
  // `unknown`, not shared_expired — an admin's shared credential is not the
  // thing that expired here).
  describe("per-principal model_access states (Appendix A finding 2 + 2b)", () => {
    it("live -> signed_in: chip 'Your AWS sign-in', no CTA, done", () => {
      render(
        <YourModelKey
          llmReady={false}
          mine={[]}
          harnesses={perUserHarness}
          modelAccess={{ state: "live" }}
          variant="default"
          onChanged={() => {}}
        />,
      );
      expect(screen.getByText(T.SIGNED_IN_CHIP)).toBeInTheDocument();
      expect(screen.getByText(T.SIGNED_IN_BODY)).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).not.toBeInTheDocument();
    });

    it("expiring -> expiring: chip 'Your AWS sign-in · Expiring' + the sign-in button", async () => {
      const onSignInAws = vi.fn();
      const user = userEvent.setup();
      render(
        <YourModelKey
          llmReady={false}
          mine={[]}
          harnesses={perUserHarness}
          modelAccess={{ state: "expiring" }}
          onSignInAws={onSignInAws}
          variant="default"
          onChanged={() => {}}
        />,
      );
      expect(screen.getByText(T.EXPIRING_CHIP)).toBeInTheDocument();
      await user.click(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
      expect(onSignInAws).toHaveBeenCalledTimes(1);
    });

    it("not_configured -> not_signed_in: chip 'Not signed in', body, sign-in button, NOT done, no reveal", () => {
      render(
        <YourModelKey
          llmReady={true}
          mine={[]}
          harnesses={perUserHarness}
          modelAccess={{ state: "not_configured" }}
          variant="default"
          onChanged={() => {}}
        />,
      );
      expect(screen.getByText(T.NOT_SIGNED_IN_CHIP)).toBeInTheDocument();
      expect(screen.getByText(T.NOT_SIGNED_IN_BODY)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeInTheDocument();
      expect(screen.queryByText("Done")).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: T.USE_OWN_KEY })).not.toBeInTheDocument();
      expect(screen.queryByPlaceholderText("sk-ant-…")).not.toBeInTheDocument();
    });

    it("expired_signin -> not_signed_in: same as not_configured", () => {
      render(
        <YourModelKey
          llmReady={true}
          mine={[]}
          harnesses={perUserHarness}
          modelAccess={{ state: "expired_signin" }}
          variant="default"
          onChanged={() => {}}
        />,
      );
      expect(screen.getByText(T.NOT_SIGNED_IN_CHIP)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeInTheDocument();
    });

    it("shared_expired (shared/none): reuses AGENTS.MODEL_ACCESS_SHARED_EXPIRED chip, NOT done, no button, reveal shown", () => {
      render(
        <YourModelKey
          llmReady={false}
          mine={[]}
          modelAccess={{ state: "shared_expired" }}
          variant="default"
          onChanged={() => {}}
        />,
      );
      expect(screen.getByText(AGENTS.MODEL_ACCESS_SHARED_EXPIRED)).toBeInTheDocument();
      expect(screen.getByText(T.SHARED_EXPIRED_BODY)).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).not.toBeInTheDocument();
      expect(screen.getByRole("button", { name: T.USE_OWN_KEY })).toBeInTheDocument();
    });
  });

  // Appendix A finding 2, plan item 5 — a member who somehow still holds
  // `mine` for the shared secret name (a stale write from before the roster
  // switched them to per_user) must not see "Your key" over a lane that can
  // never read it: hasOwn is IGNORED under per_user.
  it("hasOwn under a per_user row is ignored: still 'Not signed in', NOT done, reveal absent", () => {
    render(
      <YourModelKey
        llmReady={true}
        mine={["anthropic-api-key"]}
        harnesses={perUserHarness}
        modelAccess={{ state: "not_configured" }}
        variant="default"
        onChanged={() => {}}
      />,
    );
    expect(screen.getByText(T.NOT_SIGNED_IN_CHIP)).toBeInTheDocument();
    expect(screen.queryByText("Done")).not.toBeInTheDocument();
    expect(screen.queryByText("••••••••••••")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: T.USE_OWN_KEY })).not.toBeInTheDocument();
  });

  // Negative control for the reveal-hidden assertion above: a shared/api-key
  // row (no per_user credential_source) keeps today's "Use my own key
  // instead" reveal.
  it("negative control: reveal IS present under a shared/api-key row", () => {
    render(<YourModelKey llmReady={true} mine={[]} variant="default" onChanged={() => {}} />);
    expect(screen.getByRole("button", { name: T.USE_OWN_KEY })).toBeInTheDocument();
  });

  it("Save calls onChanged so the parent (single source of truth for `mine`) refetches", async () => {
    const onChanged = vi.fn();
    const user = userEvent.setup();
    render(<YourModelKey llmReady={false} mine={[]} variant="default" onChanged={onChanged} />);

    await user.type(screen.getByPlaceholderText("sk-ant-…"), "sk-ant-abcdefgh");
    await user.click(screen.getByRole("button", { name: "Save key" }));

    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    // No local re-fetch of its own — setSecret + the parent's callback is the
    // only round trip.
    expect(setSecretMock).toHaveBeenCalledWith("anthropic-api-key", "sk-ant-abcdefgh");
  });

  it("refused state: a 400 from the server sets the inline message and aria-invalid, and never calls onChanged", async () => {
    setSecretMock.mockRejectedValue(new HttpError(400, "secret too short"));
    const onChanged = vi.fn();
    const user = userEvent.setup();
    render(<YourModelKey llmReady={false} mine={[]} variant="default" onChanged={onChanged} />);

    const input = screen.getByPlaceholderText("sk-ant-…");
    await user.type(input, "short");
    await user.click(screen.getByRole("button", { name: "Save key" }));

    await waitFor(() => expect(screen.getByText("Keys shorter than 8 characters are refused.")).toBeInTheDocument());
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(onChanged).not.toHaveBeenCalled();
  });

  it("Remove calls onChanged on success", async () => {
    const onChanged = vi.fn();
    const user = userEvent.setup();
    render(<YourModelKey llmReady={false} mine={["anthropic-api-key"]} variant="outline" onChanged={onChanged} />);

    await user.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    expect(deleteSecretMock).toHaveBeenCalledWith("anthropic-api-key");
  });

  it("a failed Remove shows an inline error, calls no onChanged, and leaves the key showing as set", async () => {
    deleteSecretMock.mockRejectedValue(new Error("network"));
    const onChanged = vi.fn();
    const user = userEvent.setup();
    render(<YourModelKey llmReady={false} mine={["anthropic-api-key"]} variant="outline" onChanged={onChanged} />);

    await user.click(screen.getByRole("button", { name: "Remove" }));

    await waitFor(() => expect(screen.getByText("Couldn't remove this key.")).toBeInTheDocument());
    expect(onChanged).not.toHaveBeenCalled();
    // Still "set" — mine is unchanged (owned by the parent, which never
    // refetched because onChanged was never called).
    expect(screen.getByText("••••••••••••")).toBeInTheDocument();
  });

  it("unreachable (known=false) shows no Done/Provided chip regardless of mine", () => {
    render(
      <YourModelKey llmReady={true} mine={["anthropic-api-key"]} known={false} variant="default" onChanged={() => {}} />,
    );
    expect(screen.queryByText("Done")).not.toBeInTheDocument();
    // The masked-value content is still honest (it's a fact from a DIFFERENT,
    // independently-successful fetch) — only the done badge is suppressed.
    expect(screen.getByText("••••••••••••")).toBeInTheDocument();
  });
});

// X3-F3 — this pane is the ONE write path a member has, and it hardcoded
// `anthropic-api-key`. On a codex-only roster an anthropic key can never be
// used (the capability matrix marks it impossible for that harness), so the
// member stored a key that nothing would ever read. The name follows the org's
// agent roster; /secrets is untouched.
describe("YourModelKey — the secret name follows the org's agent roster", () => {
  const roster = (...ids: string[]) =>
    ids.map((id) => ({ id, display: id, has_gateway: true, has_login: true, enabled: true }));

  it("a codex-only roster writes and removes openai-api-key", async () => {
    const user = userEvent.setup();
    const { unmount } = render(
      <YourModelKey llmReady={false} mine={[]} variant="default" harnesses={roster("codex-cli")} onChanged={() => {}} />,
    );
    expect(screen.getByText("openai-api-key")).toBeInTheDocument();
    await user.type(screen.getByPlaceholderText("sk-…"), "sk-proj-abcdefgh");
    await user.click(screen.getByRole("button", { name: "Save key" }));
    await waitFor(() => expect(setSecretMock).toHaveBeenCalledWith("openai-api-key", "sk-proj-abcdefgh"));
    unmount();

    render(
      <YourModelKey
        llmReady={false}
        mine={["openai-api-key"]}
        variant="outline"
        harnesses={roster("codex-cli")}
        onChanged={() => {}}
      />,
    );
    await user.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(deleteSecretMock).toHaveBeenCalledWith("openai-api-key"));
  });

  it("negative control: a claude roster, and no roster at all, stay on anthropic-api-key", () => {
    const { unmount } = render(
      <YourModelKey
        llmReady={false}
        mine={[]}
        variant="default"
        harnesses={roster("claude-code", "codex-cli")}
        onChanged={() => {}}
      />,
    );
    expect(screen.getByText("anthropic-api-key")).toBeInTheDocument();
    unmount();
    render(<YourModelKey llmReady={false} mine={[]} variant="default" onChanged={() => {}} />);
    expect(screen.getByText("anthropic-api-key")).toBeInTheDocument();
  });
});
