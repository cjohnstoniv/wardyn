/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { act } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import {
  extractSetupToken,
  extractAuthUrl,
  extractFailSentence,
  isLikelyStartUrl,
  serverConfirmsCapture,
  HarnessLoginPane,
  loginFlow,
} from "./harness-login-pane";
import { runs as runsApiMocked } from "../../../lib/api/runs";
import type { SetupStatus } from "../../../lib/types";

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
const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) } }));

describe("HarnessLoginPane — the consent gate", () => {
  beforeEach(() => {
    harnessLoginMock.mockReset().mockResolvedValue("run-123");
    lastAttachOutput = undefined;
    vi.mocked(runsApiMocked.killRun).mockReset().mockResolvedValue(undefined);
    getSetupStatusMock.mockReset();
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

  // S-13 (blind security review, lens-S.md): the PTY doneMarker is
  // sandbox-forgeable by construction — a replaced login image can print it
  // with no real capture behind it. The pane must corroborate against the
  // server's own /setup/status before it claims a capture or calls onDone.
  describe("the helper's success marker is corroborated against the server (S-13)", () => {
    async function attachAwsRun(onDone = vi.fn(), onCancel = vi.fn()) {
      render(<HarnessLoginPane provider="aws" startURLManaged onDone={onDone} onCancel={onCancel} />);
      await userEvent.click(screen.getByRole("button", { name: /start login/i }));
      await screen.findByTestId("fake-terminal");
      return { onDone, onCancel };
    }

    // Red: a forged doneMarker with no server-side corroboration must NOT
    // call onDone — it must land on the error phase with the mismatch
    // sentence and kill the run, exactly like a real refusal would.
    it("a forged marker with no server-side capture does not call onDone and shows the mismatch error", async () => {
      getSetupStatusMock.mockResolvedValue({ harness: [], model_access: undefined } as unknown as SetupStatus);
      const { onDone } = await attachAwsRun();

      await act(async () => lastAttachOutput?.("wardyn: aws sso credential captured\n"));
      await act(async () => {}); // flush the getSetupStatus microtask

      const alertBox = await screen.findByRole("alert");
      expect(alertBox).toHaveTextContent("The sandbox reported a capture the server does not have — sign in again.");
      expect(runsApiMocked.killRun).toHaveBeenCalledWith("run-123");
      expect(onDone).not.toHaveBeenCalled();
      expect(screen.queryByText(/session captured/i)).not.toBeInTheDocument();
    });

    // Positive control: the same marker, but the server independently agrees
    // AND names THIS run as the source of the stored credential (R-1) — onDone
    // fires and the pane reports done.
    it("a marker the server corroborates via a harness row stamped with THIS run calls onDone", async () => {
      getSetupStatusMock.mockResolvedValue({
        harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }],
      } as unknown as SetupStatus);
      const { onDone } = await attachAwsRun();

      await act(async () => lastAttachOutput?.("wardyn: aws sso credential captured\n"));
      await act(async () => {});

      expect(onDone).toHaveBeenCalledTimes(1);
      expect(runsApiMocked.killRun).toHaveBeenCalledWith("run-123");
      expect(screen.queryByRole("alert")).toBeNull();
    });

    // R-1, the reconnect case the product's own "re-run the login" fix line
    // creates: a PREVIOUS sign-in's credential is sitting there, so both
    // presence legs agree — but it is not THIS run's capture, and a forged
    // marker must not be re-admitted by it.
    it("a forged marker over a PREVIOUS run's credential is refused", async () => {
      getSetupStatusMock.mockResolvedValue({
        harness: [{ provider: "aws", captured: true, source_run_id: "run-000-earlier" }],
        model_access: { state: "live" },
      } as unknown as SetupStatus);
      const { onDone } = await attachAwsRun();

      await act(async () => lastAttachOutput?.("wardyn: aws sso credential captured\n"));
      await act(async () => {});

      const alertBox = await screen.findByRole("alert");
      expect(alertBox).toHaveTextContent("The sandbox reported a capture the server does not have — sign in again.");
      expect(onDone).not.toHaveBeenCalled();
    });

    // The aws flow also corroborates via this caller's own model_access
    // (a per_user row's sign-in can land there before the harness row updates).
    it("a marker the server corroborates via model_access.state=live also calls onDone", async () => {
      getSetupStatusMock.mockResolvedValue({
        harness: [],
        model_access: { state: "live" },
      } as unknown as SetupStatus);
      const { onDone } = await attachAwsRun();

      await act(async () => lastAttachOutput?.("wardyn: aws sso credential captured\n"));
      await act(async () => {});

      expect(onDone).toHaveBeenCalledTimes(1);
      expect(screen.queryByRole("alert")).toBeNull();
    });

    // Fail-closed: a getSetupStatus rejection (network error, 401 propagated)
    // is treated the same as a disagreement — never assume the marker was
    // honest because the corroboration check itself failed.
    it("fails closed when the status fetch itself rejects", async () => {
      getSetupStatusMock.mockRejectedValue(new Error("network error"));
      const { onDone } = await attachAwsRun();

      await act(async () => lastAttachOutput?.("wardyn: aws sso credential captured\n"));
      await act(async () => {});

      const alertBox = await screen.findByRole("alert");
      expect(alertBox).toHaveTextContent("The sandbox reported a capture the server does not have — sign in again.");
      expect(onDone).not.toHaveBeenCalled();
    });

    // R-9: this is how the real client behaves for a 5xx or a dropped socket —
    // getSetupStatus RESOLVES the synthetic READY_FALLBACK (`unreachable:true`,
    // no harness, no model_access), it does not throw. The rejection case above
    // passes for the right reason only by accident, so the realistic transient
    // path gets its own pin.
    //
    // R-3: an honest capture DID land; the check is what failed. The pane must
    // not print the accusation — it retries once, then says it could not reach
    // the server.
    it("an unreachable status check retries once and then says SO — never that the server does not have it", async () => {
      getSetupStatusMock.mockResolvedValue({ unreachable: true, ready: true } as unknown as SetupStatus);
      const { onDone } = await attachAwsRun();

      await act(async () => lastAttachOutput?.("wardyn: aws sso credential captured\n"));

      const alertBox = await screen.findByRole("alert", {}, { timeout: 3000 });
      expect(alertBox).toHaveTextContent("Wardyn couldn't reach the server to verify this sign-in — try again.");
      expect(alertBox).not.toHaveTextContent("the server does not have");
      expect(getSetupStatusMock).toHaveBeenCalledTimes(2);
      expect(screen.getByRole("button", { name: /try again/i })).toBeInTheDocument();
      expect(onDone).not.toHaveBeenCalled();
    });

    // The retry is not decoration: a blip on the FIRST read must not cost an
    // honest sign-in its run.
    it("an unreachable first read followed by a corroborating retry still calls onDone", async () => {
      getSetupStatusMock
        .mockResolvedValueOnce({ unreachable: true, ready: true } as unknown as SetupStatus)
        .mockResolvedValueOnce({
          harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }],
        } as unknown as SetupStatus);
      const { onDone } = await attachAwsRun();

      await act(async () => lastAttachOutput?.("wardyn: aws sso credential captured\n"));

      await waitFor(() => expect(onDone).toHaveBeenCalledTimes(1), { timeout: 3000 });
      expect(screen.queryByRole("alert")).toBeNull();
    });

    // R-7: the login sandbox must not outlive the marker by a round trip — the
    // credential is already stored by the time the helper prints it, and
    // ssotoken.go's already_captured latch covers a repeat.
    it("kills the login run BEFORE it waits on the corroboration round trip", async () => {
      let release: (v: SetupStatus) => void = () => {};
      getSetupStatusMock.mockImplementation(() => new Promise<SetupStatus>((res) => (release = res)));
      await attachAwsRun();

      await act(async () => lastAttachOutput?.("wardyn: aws sso credential captured\n"));
      // Still in flight — and the run is already gone.
      expect(runsApiMocked.killRun).toHaveBeenCalledWith("run-123");
      // R-8: the spinner covers the round trip rather than leaving the operator
      // on a terminal that has quietly stopped scrolling.
      expect(screen.getByTestId("capture-verifying-note")).toBeInTheDocument();

      await act(async () => {
        release({ harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }] } as unknown as SetupStatus);
      });
    });
  });
});

// S-13: serverConfirmsCapture is the pure predicate the component's
// confirmCapture wires to getSetupStatus — covered directly so every branch
// (harness row, model_access state, and the anthropic flow's narrower rule)
// is pinned without going through a rendered pane.
describe("serverConfirmsCapture", () => {
  function status(overrides: Partial<SetupStatus>): SetupStatus {
    return {
      ready: true,
      checks: [],
      auth: { mode: "local", local_loopback: true },
      runner: { driver: "docker", confinement_classes: [] },
      providers: [],
      secrets: { present: [], github_app: false },
      age_key: { durable: false },
      has_runs: false,
      platform: { os: "linux", wsl: false },
      ...overrides,
    };
  }

  // R-1: the ONE fact that says "this sign-in captured something" rather than
  // "a credential exists". source_run_id is stamped server-side from the login
  // run's own token claims (ssotoken.go), so the sandbox cannot write it.
  it("aws: confirmed by a harness row stamped with THIS run", () => {
    expect(
      serverConfirmsCapture(status({ harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }] }), "aws", "run-123"),
    ).toBe(true);
  });

  // R-1, the reconnect: a PREVIOUS sign-in's credential satisfies every
  // presence predicate, and is not this run's capture.
  it("aws: a row from a PREVIOUS run is refused even when model_access is live", () => {
    expect(
      serverConfirmsCapture(
        status({ harness: [{ provider: "aws", captured: true, source_run_id: "run-earlier" }], model_access: { state: "live" } }),
        "aws",
        "run-123",
      ),
    ).toBe(false);
  });

  // R-10: the MEMBER-REDACTED shape — {provider, captured, expired,
  // source_run_id} and nothing else (setup.go's redactSetupStatusForMember).
  it("aws: the member-redacted row shape confirms on its own", () => {
    expect(
      serverConfirmsCapture(
        status({ harness: [{ provider: "aws", captured: true, expired: false, source_run_id: "run-123" }] }),
        "aws",
        "run-123",
      ),
    ).toBe(true);
  });

  // R-2: for aws the RULE is live/expiring on model_access. A harness row is a
  // presence bit that stays true for a dead credential, so on its own it only
  // widens what a forged marker can land on.
  it("aws: a presence-only harness row is NOT enough — model_access decides", () => {
    expect(serverConfirmsCapture(status({ harness: [{ provider: "aws", captured: true }] }), "aws", "run-123")).toBe(false);
    expect(
      serverConfirmsCapture(
        status({ harness: [{ provider: "aws", captured: true }], model_access: { state: "live" } }),
        "aws",
        "run-123",
      ),
    ).toBe(true);
  });

  // R-10: an EXPIRED row is exactly the shape the dropped leg used to admit.
  it("aws: an expired harness row confirms nothing", () => {
    expect(
      serverConfirmsCapture(
        status({ harness: [{ provider: "aws", captured: true, expired: true }], model_access: { state: "expired_signin" } }),
        "aws",
        "run-123",
      ),
    ).toBe(false);
  });

  it("aws: confirmed by model_access state live or expiring alone", () => {
    expect(serverConfirmsCapture(status({ model_access: { state: "live" } }), "aws", "run-123")).toBe(true);
    expect(serverConfirmsCapture(status({ model_access: { state: "expiring" } }), "aws", "run-123")).toBe(true);
  });

  it("aws: not confirmed when neither the harness row nor model_access agrees", () => {
    expect(
      serverConfirmsCapture(
        status({ harness: [{ provider: "aws", captured: false }], model_access: { state: "expired_signin" } }),
        "aws",
        "run-123",
      ),
    ).toBe(false);
    expect(serverConfirmsCapture(status({}), "aws", "run-123")).toBe(false);
  });

  it("anthropic: confirmed only by its own harness row — model_access never counts for it", () => {
    expect(serverConfirmsCapture(status({ harness: [{ provider: "anthropic", captured: true }] }), "anthropic", "run-123")).toBe(true);
    expect(serverConfirmsCapture(status({ model_access: { state: "live" } }), "anthropic", "run-123")).toBe(false);
  });

  it("a captured row for the OTHER provider does not confirm this one", () => {
    expect(serverConfirmsCapture(status({ harness: [{ provider: "anthropic", captured: true }] }), "aws", "run-123")).toBe(false);
  });
});
