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
import { AGENTS, modelAccessChipBare } from "../../../lib/workspace-providers-copy";
import type { SetupHarnessTool } from "../../../lib/types";

// U-13 (a11y): the two "Sign in to AWS" buttons carry distinct accessible
// names (the visible text plus the section they are in), so a lookup by the
// visible name is a prefix match — the same query, still by what the button
// says, and the exact aria-labels are pinned in their own case below.
const SIGN_IN_AWS_NAME = new RegExp(`^${AGENTS.SIGN_IN_AWS}`);


// Appendix A finding 2 — a per_user roster row (the wire shape a real
// bedrock_sso lane sends: modelKeyProvider's harnesses.find picks the first
// enabled row in the server's catalog order, and this test's id matches the
// default claude-code key).
const perUserHarness: SetupHarnessTool[] = [
  {
    id: "claude-code",
    display: "Claude Code",
    has_gateway: true,
    has_login: true,
    enabled: true,
    credential_source: "per_user",
    mechanism: "bedrock_sso",
  },
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

  // Appendix A finding 2 — five per-principal states through the card. Each
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
      expect(screen.queryByRole("button", { name: SIGN_IN_AWS_NAME })).not.toBeInTheDocument();
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
      // The expiring body, not just the chip.
      expect(screen.getByText(T.SIGNED_IN_BODY)).toBeInTheDocument();
      await user.click(screen.getByRole("button", { name: SIGN_IN_AWS_NAME }));
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
      expect(screen.getByRole("button", { name: SIGN_IN_AWS_NAME })).toBeInTheDocument();
      expect(screen.queryByText("Done")).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: T.USE_OWN_KEY })).not.toBeInTheDocument();
      expect(screen.queryByPlaceholderText("sk-ant-…")).not.toBeInTheDocument();
    });

    // U-10 — supersedes "same as not_configured": an expired
    // session is not "nothing is configured for you". The chip is the chip row's
    // own label for the state (bare inside this card), and the body says what
    // actually happened — which is also what the server's action line on the
    // same page says.
    it("expired_signin -> not_signed_in with its OWN chip and body, never 'nothing is configured'", () => {
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
      expect(screen.getByText(modelAccessChipBare("expired_signin")!)).toBeInTheDocument();
      expect(screen.getByText(T.EXPIRED_SIGNIN_BODY)).toBeInTheDocument();
      expect(screen.queryByText(T.NOT_SIGNED_IN_CHIP)).not.toBeInTheDocument();
      expect(screen.queryByText(T.NOT_SIGNED_IN_BODY)).not.toBeInTheDocument();
      expect(screen.getByRole("button", { name: SIGN_IN_AWS_NAME })).toBeInTheDocument();
    });

    // U-10's other half: not_configured keeps today's chip and body exactly.
    it("not_configured keeps the not-signed-in chip and body", () => {
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
      expect(screen.queryByText(T.EXPIRED_SIGNIN_BODY)).not.toBeInTheDocument();
    });

    // U-15: the chip is the same label as the chip row's, without the
    // "Model access · " qualifier — this card's own heading is the subject.
    it("shared_expired (shared/none): the bare shared-expired chip, NOT done, no button, reveal shown", () => {
      render(
        <YourModelKey
          llmReady={false}
          mine={[]}
          modelAccess={{ state: "shared_expired" }}
          variant="default"
          onChanged={() => {}}
        />,
      );
      expect(screen.getByText(modelAccessChipBare("shared_expired")!)).toBeInTheDocument();
      expect(screen.queryByText(AGENTS.MODEL_ACCESS_SHARED_EXPIRED)).not.toBeInTheDocument();
      expect(screen.getByText(T.SHARED_EXPIRED_BODY)).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: SIGN_IN_AWS_NAME })).not.toBeInTheDocument();
      expect(screen.getByRole("button", { name: T.USE_OWN_KEY })).toBeInTheDocument();
    });

    // FIX PASS 1 — not_applicable is emitted only for the
    // admin-token principal on an enabled per_user row (awsSSOScopeIsMechanism,
    // internal/api/modelaccess.go): real traffic, not a skew artifact. The
    // result stays `unknown`, but the body is now PER_PERSON_NA_BODY,
    // never a bare title.
    it("not_applicable under per_user -> PER_PERSON_NA_BODY, no chip, no button, no form, reveal hidden", () => {
      render(
        <YourModelKey
          llmReady={true}
          mine={[]}
          harnesses={perUserHarness}
          modelAccess={{ state: "not_applicable" }}
          variant="default"
          onChanged={() => {}}
        />,
      );
      expect(screen.getByText(T.PER_PERSON_NA_BODY)).toBeInTheDocument();
      expect(screen.queryByText("Done")).not.toBeInTheDocument();
      expect(screen.queryByText(T.PROVIDED_CHIP)).not.toBeInTheDocument();
      expect(screen.queryByText(T.PROVIDED_BODY)).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: SIGN_IN_AWS_NAME })).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: T.USE_OWN_KEY })).not.toBeInTheDocument();
      expect(screen.queryByPlaceholderText("sk-ant-…")).not.toBeInTheDocument();
    });

    // PER_PERSON_NA_BODY is the `not_applicable` answer,
    // and it is also the fallthrough for any absent or unrecognised state under
    // a per_user row — telling a member whose state could not be read that there
    // was nothing to set up, beside a lede saying model access uses their own AWS
    // sign-in. Unknown claims nothing.
    it.each([["expired_renewable"], [undefined]])(
      "per_user + state %s renders NO body at all (U-14)",
      (state) => {
        render(
          <YourModelKey
            llmReady={true}
            mine={[]}
            harnesses={perUserHarness}
            modelAccess={state ? { state } : undefined}
            variant="default"
            onChanged={() => {}}
          />,
        );
        expect(screen.queryByText(T.PER_PERSON_NA_BODY)).not.toBeInTheDocument();
        expect(screen.queryByText(T.ADMIN_NOT_READY_BODY)).not.toBeInTheDocument();
        expect(screen.queryByText(T.PROVIDED_BODY)).not.toBeInTheDocument();
        expect(screen.queryByPlaceholderText("sk-ant-…")).not.toBeInTheDocument();
      },
    );
  });

  // U-13 (a11y) — two buttons on the page say "Sign in to AWS" with the same
  // visible name, and the card's opens a pane in a different card above it.
  describe("the card's own Sign in to AWS button (U-13)", () => {
    const signInProps = {
      llmReady: true,
      mine: [] as string[],
      harnesses: perUserHarness,
      modelAccess: { state: "not_configured" },
      variant: "default" as const,
      onChanged: () => {},
    };

    it("carries its own accessible name, which still starts with the visible text", () => {
      render(<YourModelKey {...signInProps} />);
      const button = screen.getByRole("button", { name: T.SIGN_IN_AWS_ARIA_CARD });
      expect(button).toHaveTextContent(AGENTS.SIGN_IN_AWS);
      expect(T.SIGN_IN_AWS_ARIA_CARD.startsWith(AGENTS.SIGN_IN_AWS)).toBe(true);
    });

    it("is hidden while the pane it opens is already open", () => {
      render(<YourModelKey {...signInProps} signInOpen />);
      expect(screen.queryByRole("button", { name: SIGN_IN_AWS_NAME })).not.toBeInTheDocument();
    });
  });

  // FIX PASS 1 — a shared row (no per_user
  // credential_source) whose declared mechanism is Bedrock: mechanismSatisfied
  // refuses a member's own API key on any Bedrock roster row regardless of
  // credential_source, so this band behaves like per_user for hasOwn/reveal
  // purposes even though `status.model_access` here is the deployment-wide
  // answer (the caller is not graded per-principal on a shared row).
  describe("shared row with a Bedrock mechanism", () => {
    // ticket: REVIEW-1.md I1/R2(b)
    const sharedBedrockHarness: SetupHarnessTool[] = [
      { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true, mechanism: "bedrock_sso" },
    ];

    it("hasOwn is ignored: still shows shared_expired, not 'Your key'", () => {
      render(
        <YourModelKey
          llmReady={false}
          mine={["anthropic-api-key"]}
          harnesses={sharedBedrockHarness}
          modelAccess={{ state: "shared_expired" }}
          variant="default"
          onChanged={() => {}}
        />,
      );
      expect(screen.getByText(modelAccessChipBare("shared_expired")!)).toBeInTheDocument();
      expect(screen.getByText(T.SHARED_EXPIRED_BODY)).toBeInTheDocument();
      expect(screen.queryByText("Done")).not.toBeInTheDocument();
      expect(screen.queryByText("••••••••••••")).not.toBeInTheDocument();
      // Reveal is hidden here (unlike the plain shared_expired case above) —
      // a member's own key is useless under Bedrock regardless of credential_source.
      expect(screen.queryByRole("button", { name: T.USE_OWN_KEY })).not.toBeInTheDocument();
    });

    it("llmReady true, anything else -> provided chip/body, reveal HIDDEN", () => {
      // ticket: I1
      render(
        <YourModelKey
          llmReady={true}
          mine={[]}
          harnesses={sharedBedrockHarness}
          modelAccess={{ state: "live" }}
          variant="default"
          onChanged={() => {}}
        />,
      );
      expect(screen.getByText(T.PROVIDED_CHIP)).toBeInTheDocument();
      expect(screen.getByText(T.PROVIDED_BODY)).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: T.USE_OWN_KEY })).not.toBeInTheDocument();
      expect(screen.queryByPlaceholderText("sk-ant-…")).not.toBeInTheDocument();
    });

    it("llmReady false, anything else -> ADMIN_NOT_READY_BODY, no chip, no button, no form", () => {
      render(
        <YourModelKey
          llmReady={false}
          mine={[]}
          harnesses={sharedBedrockHarness}
          modelAccess={{ state: "not_applicable" }}
          variant="default"
          onChanged={() => {}}
        />,
      );
      expect(screen.getByText(T.ADMIN_NOT_READY_BODY)).toBeInTheDocument();
      expect(screen.queryByText("Done")).not.toBeInTheDocument();
      expect(screen.queryByText(T.PROVIDED_CHIP)).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: SIGN_IN_AWS_NAME })).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: T.USE_OWN_KEY })).not.toBeInTheDocument();
      expect(screen.queryByPlaceholderText("sk-ant-…")).not.toBeInTheDocument();
    });
  });

  // Appendix A finding 2, plan item 5 — a member who somehow still holds
  // `mine` for the shared secret name (a stale write from before the roster
  // switched them to per_user) must not see "Your key" over a lane that can
  // never read it: hasOwn is ignored under per_user.
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
    // The masked-value content is still honest (it's a fact from a different,
    // independently-successful fetch) — only the done badge is suppressed.
    expect(screen.getByText("••••••••••••")).toBeInTheDocument();
  });
});

// X3-F3 — this pane is the one write path a member has, and it hardcoded
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
