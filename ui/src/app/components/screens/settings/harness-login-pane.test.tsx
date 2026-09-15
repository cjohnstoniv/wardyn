/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { act } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { extractSetupToken, extractAuthUrl, extractFailSentence, isLikelyStartUrl, HarnessLoginPane, loginFlow } from "./harness-login-pane";
import { runs as runsApiMocked } from "../../../lib/api/runs";

// A realistic setup-token body: sk-ant-oat<2 digits>-<long url-safe blob>.
const TOKEN = "sk-ant-oat01-" + "A".repeat(60) + "-_" + "b3".repeat(10);

describe("extractSetupToken", () => {
  it("captures a complete token followed by a newline", () => {
    expect(extractSetupToken(`Your token:\n${TOKEN}\n`)).toBe(TOKEN);
  });

  it("captures a token even when ANSI/reset codes follow it", () => {
    expect(extractSetupToken(`${TOKEN}\x1b[0m\r\n`)).toBe(TOKEN);
  });

  it("does NOT capture a token still streaming at the buffer's end", () => {
    // No trailing char yet → treat as truncated, wait for more output.
    expect(extractSetupToken(`prefix ${TOKEN}`)).toBeNull();
  });

  it("returns null when there is no token", () => {
    expect(extractSetupToken("just some\r\nterminal output\n")).toBeNull();
  });

  it("ignores a too-short lookalike (not a real token)", () => {
    expect(extractSetupToken("sk-ant-oat01-short\n")).toBeNull();
  });

  it("finds the token embedded in noisy multi-line output", () => {
    const out = `\x1b[32m✓\x1b[0m Authenticated\r\nCopy this token:\r\n  ${TOKEN}  \r\nDone.`;
    expect(extractSetupToken(out)).toBe(TOKEN);
  });
});

describe("extractAuthUrl", () => {
  it("captures a claude.ai OAuth URL followed by a newline", () => {
    const url = "https://claude.ai/oauth/authorize?code=true&client_id=abc123&scope=user";
    expect(extractAuthUrl(`Visit:\r\n${url}\r\n`)).toBe(url);
  });

  it("captures a console.anthropic.com auth URL", () => {
    const url = "https://console.anthropic.com/oauth/authorize?x=1";
    expect(extractAuthUrl(`${url}\n`)).toBe(url);
  });

  it("strips trailing punctuation", () => {
    const url = "https://claude.ai/oauth/authorize?code=true";
    expect(extractAuthUrl(`Open (${url}).\n`)).toBe(url);
  });

  it("does NOT capture a URL still streaming at the buffer's end", () => {
    expect(extractAuthUrl("go to https://claude.ai/oauth/authorize?code=tru")).toBeNull();
  });

  it("ignores the token-exchange host (api.anthropic.com) and unrelated URLs", () => {
    expect(extractAuthUrl("POST https://api.anthropic.com/v1/oauth/token \n")).toBeNull();
    expect(extractAuthUrl("see https://example.com/docs \n")).toBeNull();
  });

  it("captures a full ~200-char OAuth URL on one line (login PTY is forced wide so it never wraps)", () => {
    // The login flow forces LOGIN_PTY_COLS so claude prints this on a single line;
    // response_type=code (dropped by the old narrow-PTY wrap bug) survives intact.
    const url =
      "https://claude.ai/oauth/authorize?response_type=code&client_id=abcdef0123456789&redirect_uri=https%3A%2F%2Fconsole.anthropic.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile&code_challenge=Zm9vYmFyYmF6cXV4&code_challenge_method=S256&state=deadbeefcafef00d";
    expect(extractAuthUrl(`Visit:\r\n${url}\r\nPaste the code here:\r\n`)).toBe(url);
    expect(new URL(extractAuthUrl(`\r\n${url}\r\n`)!).searchParams.get("response_type")).toBe("code");
  });
});

// wardyn-aws-sso prints `<failMarker> <sentence>\n` on a refused capture — a
// wrong-account pin, a portal error — and never prints the doneMarker in that
// case, so without this the pane just spins on "waiting" forever (Appendix A
// finding 1's fail-fast ask, pane half).
describe("extractFailSentence", () => {
  const MARKER = "wardyn: aws sso credential rejected:";

  it("returns the sentence once the line has finished printing", () => {
    expect(extractFailSentence(`${MARKER} the pinned account is not entitled to this session.\n`, MARKER)).toBe(
      "the pinned account is not entitled to this session.",
    );
  });

  it("does NOT return a still-streaming line (no trailing newline yet)", () => {
    expect(extractFailSentence(`${MARKER} the pinned acco`, MARKER)).toBeNull();
  });

  it("returns null when the marker never printed", () => {
    expect(extractFailSentence("some other terminal output\n", MARKER)).toBeNull();
  });

  it("strips a trailing carriage return (PTY line endings)", () => {
    expect(extractFailSentence(`${MARKER} refused.\r\n`, MARKER)).toBe("refused.");
  });
});

describe("isLikelyStartUrl", () => {
  it("accepts a real AWS access portal URL", () => {
    expect(isLikelyStartUrl("https://my-org.awsapps.com/start")).toBe(true);
    expect(isLikelyStartUrl("  https://identitycenter.amazonaws.com/ssoins-abc  ")).toBe(true);
  });

  it("rejects what the server would also reject", () => {
    expect(isLikelyStartUrl("")).toBe(false);
    expect(isLikelyStartUrl("my-org.awsapps.com/start")).toBe(false);
    expect(isLikelyStartUrl("http://my-org.awsapps.com/start")).toBe(false);
    // A newline would smuggle extra keys into the generated ~/.aws/config INI.
    expect(isLikelyStartUrl("https://\nsso_region = x")).toBe(false);
  });
});

// W12-W12-C-6 + W5-S1-7: the "done" phase's success line used to be a hardcoded
// "your Claude subscription is connected" regardless of provider, so an AWS SSO
// capture ended with the same Anthropic-only claim. doneLabel is per-provider.
describe("loginFlow doneLabel", () => {
  it("names the provider actually connected, not always Claude", () => {
    expect(loginFlow("anthropic").doneLabel).toMatch(/claude subscription/i);
    expect(loginFlow("aws").doneLabel).toMatch(/aws sso/i);
    expect(loginFlow("aws").doneLabel).not.toMatch(/claude subscription/i);
  });
});

// ─── the consent gate ────────────────────────────────────────────────────────
//
// Owner report, verbatim: "you see a dialog then all of a sudden terminal then
// all of a sudden a popup asking for auth. We should alert the user before
// this happens what to expect and what's required from them." The pane used to
// launch the sandbox ON MOUNT; now nothing happens until Start login.

// lastAttachOutput captures the onOutput callback the pane hands AttachTerminal
// on its most recent render, so a test can feed it PTY chunks directly — the
// only way to exercise handleOutput's marker-watching without a real xterm
// (which does not render in jsdom).
let lastAttachOutput: ((chunk: string) => void) | undefined;
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: React.forwardRef(function FakeTerminal(props: { onOutput?: (chunk: string) => void }) {
    lastAttachOutput = props.onOutput;
    return <div data-testid="fake-terminal" />;
  }),
}));
const harnessLoginMock = vi.fn();
vi.mock("../../../lib/api/harness-auth", () => ({
  harnessAuth: {
    harnessLogin: (...a: unknown[]) => harnessLoginMock(...a),
    harnessCredentialPaste: vi.fn(),
  },
}));
vi.mock("../../../lib/api/runs", () => ({ runs: { killRun: vi.fn() } }));

describe("HarnessLoginPane — the consent gate", () => {
  beforeEach(() => {
    harnessLoginMock.mockReset().mockResolvedValue("run-123");
    lastAttachOutput = undefined;
    vi.mocked(runsApiMocked.killRun).mockReset().mockResolvedValue(undefined);
  });

  it("launches NOTHING on mount: the intro says what to expect and what's required", () => {
    render(<HarnessLoginPane provider="anthropic" onDone={vi.fn()} onCancel={vi.fn()} />);
    expect(screen.getByTestId("login-intro")).toBeInTheDocument();
    // The two things the jump never announced: a terminal, and a browser auth.
    expect(screen.getByText(/a terminal appears here/i)).toBeInTheDocument();
    expect(screen.getByText(/sign in and approve/i)).toBeInTheDocument();
    // The requirement on the operator, stated up front.
    expect(screen.getByText(/active Claude subscription/i)).toBeInTheDocument();
    expect(harnessLoginMock).not.toHaveBeenCalled();
    expect(screen.queryByTestId("fake-terminal")).not.toBeInTheDocument();
  });

  it("Start login is the ONLY thing that launches the sandbox", async () => {
    render(<HarnessLoginPane provider="anthropic" onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));
    expect(harnessLoginMock).toHaveBeenCalledWith("anthropic", "");
    expect(await screen.findByTestId("fake-terminal")).toBeInTheDocument();
  });

  it("Cancel on the intro backs out without ever launching", async () => {
    const onCancel = vi.fn();
    render(<HarnessLoginPane provider="anthropic" onDone={vi.fn()} onCancel={onCancel} />);
    await userEvent.click(screen.getByRole("button", { name: /cancel/i }));
    expect(onCancel).toHaveBeenCalled();
    expect(harnessLoginMock).not.toHaveBeenCalled();
  });

  it("AWS keeps its start-URL gate and now states what happens next above it", () => {
    render(<HarnessLoginPane provider="aws" onDone={vi.fn()} onCancel={vi.fn()} />);
    expect(screen.getByLabelText(/aws access portal start url/i)).toBeInTheDocument();
    expect(screen.getByText(/verification page/i)).toBeInTheDocument();
    expect(harnessLoginMock).not.toHaveBeenCalled();
  });

  // V1 r2 LOW: under a per_user agent row the server signs in against the ROW's
  // stored sso_start_url and IGNORES whatever is typed here, so asking was a
  // field that could not take effect — and every member had to hunt down a URL
  // their admin had already entered.
  describe("startURLManaged — the org's portal is a fact, not a question", () => {
    it("skips the start-URL gate for the note, and launches with an empty start URL", async () => {
      render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
      expect(screen.queryByLabelText(/aws access portal start url/i)).toBeNull();
      expect(screen.getByText(AGENTS.SSO_START_URL_MANAGED)).toBeInTheDocument();
      // Straight to the consent gate, with the same "what happens next" list.
      expect(screen.getByTestId("login-intro")).toBeInTheDocument();
      expect(screen.getByText(/verification page/i)).toBeInTheDocument();

      await userEvent.click(screen.getByRole("button", { name: /start login/i }));
      expect(harnessLoginMock).toHaveBeenCalledWith("aws", "");
    });

    it("the DEFAULT is unchanged — the ordinary Settings sign-in still asks", () => {
      render(<HarnessLoginPane provider="aws" onDone={vi.fn()} onCancel={vi.fn()} />);
      expect(screen.getByLabelText(/aws access portal start url/i)).toBeInTheDocument();
      expect(screen.queryByText(AGENTS.SSO_START_URL_MANAGED)).toBeNull();
    });

    it("means nothing to a flow that never asked (anthropic)", () => {
      render(<HarnessLoginPane provider="anthropic" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
      expect(screen.getByTestId("login-intro")).toBeInTheDocument();
      expect(screen.queryByText(AGENTS.SSO_START_URL_MANAGED)).toBeNull();
    });
  });

  // Appendix A finding 1's fail-fast ask, pane half: wardyn-aws-sso prints a
  // fail marker on a refused capture (a wrong-account pin, a portal error) and
  // NEVER prints doneMarker in that case — before this latch the pane just sat
  // on "waiting" forever with no explanation, the operator's only signal a
  // terminal that stopped scrolling.
  describe("the helper's fail marker ends the wait with why", () => {
    async function attachAwsRun(onDone = vi.fn(), onCancel = vi.fn()) {
      render(<HarnessLoginPane provider="aws" startURLManaged onDone={onDone} onCancel={onCancel} />);
      await userEvent.click(screen.getByRole("button", { name: /start login/i }));
      await screen.findByTestId("fake-terminal");
      return { onDone, onCancel };
    }

    it("no marker keeps the terminal attached", async () => {
      await attachAwsRun();
      await act(async () => lastAttachOutput?.("some ordinary aws sso login chatter\n"));
      expect(screen.getByTestId("fake-terminal")).toBeInTheDocument();
      expect(screen.queryByRole("alert")).toBeNull();
    });

    // R5 (fix-first review pass): the run is already killed by this point, so
    // the terminal's socket just closes — but the SCROLLBACK (device-code
    // chatter, the portal's reply, the helper's own preceding lines) stays on
    // screen beside the alert rather than vanishing with it, since the one
    // extracted sentence is otherwise the operator's only artifact.
    it("the fail marker moves to the error phase, names why, kills the run, and keeps the scrollback", async () => {
      const { onDone } = await attachAwsRun();
      await act(async () =>
        lastAttachOutput?.("wardyn: aws sso credential rejected: the pinned account is not entitled to this session.\n"),
      );

      const alertBox = await screen.findByRole("alert");
      expect(alertBox).toHaveTextContent("the pinned account is not entitled to this session.");
      // The error phase's Try again / Cancel AND the terminal, not the
      // interactive attached-phase controls (paste boxes, its own Cancel).
      expect(screen.getByTestId("fake-terminal")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /try again/i })).toBeInTheDocument();
      expect(screen.queryByTestId("auth-url-link")).not.toBeInTheDocument();
      expect(runsApiMocked.killRun).toHaveBeenCalledWith("run-123");
      expect(onDone).not.toHaveBeenCalled();
    });

    it("never mistakes the fail marker's own line for a success", async () => {
      await attachAwsRun();
      await act(async () => lastAttachOutput?.("wardyn: aws sso credential rejected: portal timeout.\n"));
      await screen.findByRole("alert");
      expect(screen.queryByText(/session captured/i)).not.toBeInTheDocument();
    });
  });
});
