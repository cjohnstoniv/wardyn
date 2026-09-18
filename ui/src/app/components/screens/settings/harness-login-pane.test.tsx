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
  SANDBOX_REFUSAL_LEAD_IN,
  isLikelyStartUrl,
  serverConfirmsCapture,
  HarnessLoginPane,
  loginFlow,
  SELFRUN_MARKER,
  CAPTURE_NOT_CORROBORATED,
  LOGIN_SANDBOX_STARTING,
  LOGIN_SANDBOX_UNREADABLE,
} from "./harness-login-pane";
import { LOGIN_SANDBOX_READ_RETRYING, LOGIN_SANDBOX_SLOW_START } from "./login-start-wait";
import { CAPTURE_POST_RUN_GRACE_MS } from "./capture-confirm";
import { runs as runsApiMocked } from "../../../lib/api/runs";
import type { AgentRun, SetupStatus } from "../../../lib/types";

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

  // U2-05 (blind round 2, lens-U2): the 300-rune cap, the ANSI strip and the
  // marker defang all live in cmd/wardyn-aws-sso — i.e. in the binary a forged
  // login image REPLACES, which is the threat model S-13 hardened the success
  // path against. The client keeps a bound of its own so a sandbox cannot
  // paint a screenful of its own prose into Wardyn's alert.
  it("caps the sentence at 300 characters, whatever the sandbox printed", () => {
    const long = "x".repeat(5000);
    expect(extractFailSentence(`${MARKER} ${long}\n`, MARKER)).toHaveLength(300);
  });

  it("leaves a sentence within the bound untouched", () => {
    expect(extractFailSentence(`${MARKER} refused.\n`, MARKER)).toBe("refused.");
  });

  // RV-03 (review follow-up): the ANSI strip lived only in cmd/wardyn-aws-sso,
  // beside the cap U2-05 already re-applied here — same argument, same place.
  // Colour/cursor CSI and an OSC title-set are the shapes a PTY actually emits.
  it("strips ANSI CSI and OSC sequences the sandbox printed", () => {
    expect(extractFailSentence(`${MARKER} \u001b[1;31mrefused\u001b[0m.\n`, MARKER)).toBe("refused.");
    expect(extractFailSentence(`${MARKER} \u001b]0;pwned title\u0007refused.\n`, MARKER)).toBe("refused.");
  });

  // The strip runs BEFORE the cap, so escape bytes cannot spend the 300-char
  // budget on the operator's behalf.
  it("caps on visible characters, not on escape bytes", () => {
    const noisy = "\u001b[31mx\u001b[0m".repeat(400);
    expect(extractFailSentence(`${MARKER} ${noisy}\n`, MARKER)).toBe("x".repeat(300));
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
// The props of that same render — the login pane's whole job is to type ONE
// chained command into the PTY, so what it hands the terminal is a contract.
let lastAttachProps: { onOutput?: (chunk: string) => void; autoRun?: string } | undefined;
// The aws sandbox runs the chained command ITSELF now, so the pane's only
// remaining typing path is its grace-window fallback through this handle — the
// same `sendText` the pasted-code field uses. Spied, because "did the console
// type a SECOND login into a sandbox already running one" is the whole question.
const sendTextSpy = vi.fn();
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: React.forwardRef(function FakeTerminal(
    props: { onOutput?: (chunk: string) => void; autoRun?: string },
    ref: React.ForwardedRef<{ sendText: (t: string) => void }>,
  ) {
    lastAttachOutput = props.onOutput;
    lastAttachProps = props;
    React.useImperativeHandle(ref, () => ({ sendText: (t: string) => sendTextSpy(t) }), []);
    return <div data-testid="fake-terminal" />;
  }),
}));
const harnessLoginMock = vi.fn();
const harnessPasteMock = vi.fn();
vi.mock("../../../lib/api/harness-auth", () => ({
  harnessAuth: {
    harnessLogin: (...a: unknown[]) => harnessLoginMock(...a),
    harnessCredentialPaste: (...a: unknown[]) => harnessPasteMock(...a),
  },
}));
vi.mock("../../../lib/api/runs", () => ({ runs: { killRun: vi.fn(), getRun: vi.fn() } }));
const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) } }));
// review-1 S1: the watch polls the run's audit trail as a hint; defaulted to
// "no hint" so the 4 rewritten S-13 pins exercise its 30s STATUS fallback.
const listAuditMock = vi.fn();
vi.mock("../../../lib/api/audit", () => ({ audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) } }));

describe("HarnessLoginPane — the consent gate", () => {
  beforeEach(() => {
    harnessLoginMock.mockReset().mockResolvedValue("run-123");
    harnessPasteMock.mockReset().mockResolvedValue(undefined);
    lastAttachOutput = undefined;
    lastAttachProps = undefined;
    vi.mocked(runsApiMocked.killRun).mockReset().mockResolvedValue(undefined);
    // P5: the pane no longer mounts the terminal on the POST's own resolve —
    // it polls the run to RUNNING first (a 200 now precedes dispatch). Every
    // case below that wants a terminal gets a run that is already up; the
    // starting/failed shapes drive this mock themselves.
    vi.mocked(runsApiMocked.getRun)
      .mockReset()
      .mockResolvedValue({ id: "run-123", state: "RUNNING" } as AgentRun);
    getSetupStatusMock.mockReset();
    listAuditMock.mockReset().mockResolvedValue([]);
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
      // U2-05: the sentence is the SANDBOX's prose rendered inside Wardyn's
      // own warning box — a fixed Wardyn-authored lead-in names the speaker so
      // it never reads as Wardyn's own finding.
      expect(alertBox).toHaveTextContent(SANDBOX_REFUSAL_LEAD_IN);
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

  // RR-2: `saving` with no autoCapture is TWO different states. On a helper
  // flow it is the corroboration round trip (R-8's spinner). On the anthropic
  // scrape flow it is a MANUAL token paste — which keeps its own row, its own
  // Save spinner, and must never be handed a sentence about an AWS sign-in it
  // never made.
  it("a manual token paste keeps its paste row while saving — never the helper's verifying note", async () => {
    let settle: () => void = () => {};
    harnessPasteMock.mockImplementation(() => new Promise<void>((res) => (settle = () => res())));
    render(<HarnessLoginPane provider="anthropic" onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));
    await screen.findByTestId("fake-terminal");

    await userEvent.type(screen.getByLabelText("setup-token"), TOKEN);
    await userEvent.click(screen.getByRole("button", { name: /save token/i }));

    // Mid-save: the paste row is still the operator's surface.
    expect(screen.queryByTestId("capture-verifying-note")).not.toBeInTheDocument();
    expect(screen.getByLabelText("setup-token")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /save token/i })).toBeDisabled();

    await act(async () => {
      settle();
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

    // review-1 S1: fake-timer-aware click (`attachedOnFakeTimers`'s pattern).
    async function attachAwsRunUnderFakeTimers(onDone = vi.fn(), onCancel = vi.fn()) {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      render(<HarnessLoginPane provider="aws" startURLManaged onDone={onDone} onCancel={onCancel} />);
      await user.click(screen.getByRole("button", { name: /start login/i }));
      await screen.findByTestId("fake-terminal");
      return { onDone, onCancel };
    }
    async function advanceUnderFakeTimers(ms: number) {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(ms);
      });
    }
    // The watch's own bound: terminal, then the upload grace elapses.
    async function watchGivesUp() {
      vi.mocked(runsApiMocked.getRun).mockResolvedValue({ id: "run-123", state: "COMPLETED" } as AgentRun);
      await advanceUnderFakeTimers(CAPTURE_POST_RUN_GRACE_MS + 60_000);
    }

    // review-1 S1 (Codex #9): hands off to the watch instead of refusing
    // alone — only WHEN the refusal lands changes, not the property.
    it("a forged marker with no server-side capture keeps verifying, then ends in the mismatch error once the watch gives up", async () => {
      const onDone = vi.fn();
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        getSetupStatusMock.mockResolvedValue({ harness: [], model_access: undefined } as unknown as SetupStatus);
        await attachAwsRunUnderFakeTimers(onDone);

        await act(async () => lastAttachOutput?.("wardyn: aws sso credential captured\n"));
        // The short round trip's re-read window, asserted BEFORE advancing further.
        await advanceUnderFakeTimers(2_000);
        expect(screen.getByTestId("capture-verifying-note")).toBeInTheDocument();
        expect(screen.getByRole("button", { name: /cancel/i })).toBeInTheDocument();
        expect(screen.queryByRole("alert")).toBeNull();
        expect(onDone).not.toHaveBeenCalled();

        await watchGivesUp();
      } finally {
        vi.useRealTimers();
      }

      const alertBox = screen.getByRole("alert");
      expect(alertBox).toHaveTextContent(CAPTURE_NOT_CORROBORATED);
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

    // R-1: a PREVIOUS sign-in's credential agrees on both presence legs but
    // is not THIS run's capture — must not be re-admitted.
    it("a forged marker over a PREVIOUS run's credential keeps verifying, then is refused once the watch gives up", async () => {
      const onDone = vi.fn();
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        getSetupStatusMock.mockResolvedValue({
          harness: [{ provider: "aws", captured: true, source_run_id: "run-000-earlier" }],
          model_access: { state: "live" },
        } as unknown as SetupStatus);
        await attachAwsRunUnderFakeTimers(onDone);

        await act(async () => lastAttachOutput?.("wardyn: aws sso credential captured\n"));
        await advanceUnderFakeTimers(2_000);
        expect(screen.getByTestId("capture-verifying-note")).toBeInTheDocument();
        expect(screen.queryByRole("alert")).toBeNull();
        expect(onDone).not.toHaveBeenCalled();

        await watchGivesUp();
      } finally {
        vi.useRealTimers();
      }

      const alertBox = screen.getByRole("alert");
      expect(alertBox).toHaveTextContent(CAPTURE_NOT_CORROBORATED);
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

    // Fail-closed: a rejection is a disagreement, never an honest marker.
    it("fails closed when the status fetch itself rejects — keeps verifying, refused once the watch gives up", async () => {
      const onDone = vi.fn();
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        getSetupStatusMock.mockRejectedValue(new Error("network error"));
        await attachAwsRunUnderFakeTimers(onDone);

        await act(async () => lastAttachOutput?.("wardyn: aws sso credential captured\n"));
        await advanceUnderFakeTimers(2_000);
        // A THROW is an answer, not a blip: one read is all the short round
        // trip makes, asserted before the watch's own 30s fallback is due.
        expect(getSetupStatusMock).toHaveBeenCalledTimes(1);
        expect(screen.getByTestId("capture-verifying-note")).toBeInTheDocument();
        expect(screen.queryByRole("alert")).toBeNull();
        expect(onDone).not.toHaveBeenCalled();

        await watchGivesUp();
      } finally {
        vi.useRealTimers();
      }

      const alertBox = screen.getByRole("alert");
      expect(alertBox).toHaveTextContent(CAPTURE_NOT_CORROBORATED);
      expect(onDone).not.toHaveBeenCalled();
    });

    // R-9/R-3: unreachable RESOLVES (does not throw); an honest capture that
    // DID land must not be accused — retry once, then say the check failed.
    it("an unreachable status check retries once, keeps verifying, then says SO once the watch gives up", async () => {
      const onDone = vi.fn();
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        getSetupStatusMock.mockResolvedValue({ unreachable: true, ready: true } as unknown as SetupStatus);
        await attachAwsRunUnderFakeTimers(onDone);

        await act(async () => lastAttachOutput?.("wardyn: aws sso credential captured\n"));
        await advanceUnderFakeTimers(2_000);
        expect(getSetupStatusMock).toHaveBeenCalledTimes(2);
        expect(screen.getByTestId("capture-verifying-note")).toBeInTheDocument();
        expect(screen.queryByRole("alert")).toBeNull();
        expect(onDone).not.toHaveBeenCalled();

        await watchGivesUp();
      } finally {
        vi.useRealTimers();
      }

      const alertBox = screen.getByRole("alert");
      expect(alertBox).toHaveTextContent("Wardyn couldn't reach the server to verify this sign-in — try again.");
      expect(alertBox).not.toHaveTextContent("the server does not have");
      expect(screen.getByRole("button", { name: /try again/i })).toBeInTheDocument();
      expect(onDone).not.toHaveBeenCalled();
    });

    // Finding 7 (0.7.4 field report), console half: the status read ANSWERS and
    // simply does not show this run's capture yet — a lagging replica, or the
    // supersede's kill landing between the 204 and the read. The write really
    // did precede the read; it was not VISIBLE to it. Refusing on that first
    // read is what told a person whose retry worked to sign in again.
    //
    // Red on the unfixed tree: one read, one refusal, no second chance.
    it("re-reads a status that answers but does not show this run's capture yet", async () => {
      getSetupStatusMock
        .mockResolvedValueOnce({ harness: [], model_access: undefined } as unknown as SetupStatus)
        .mockResolvedValue({
          harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }],
        } as unknown as SetupStatus);
      const { onDone } = await attachAwsRun();

      await act(async () => lastAttachOutput?.("wardyn: aws sso credential captured\n"));

      await waitFor(() => expect(onDone).toHaveBeenCalledTimes(1), { timeout: 3000 });
      expect(screen.queryByRole("alert")).toBeNull();
      expect(getSetupStatusMock).toHaveBeenCalledTimes(2);
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
      // on a terminal that has quietly stopped scrolling. U2-07 (blind round
      // 2): and a screen-reader user hears it — the whole point of narrating a
      // silent round trip is lost in a note no live region announces.
      expect(screen.getByTestId("capture-verifying-note")).toBeInTheDocument();
      expect(screen.getByTestId("capture-verifying-note")).toHaveAttribute("role", "status");

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

// ─── P5: the pane waits for the sandbox instead of assuming it ───────────────
//
// POST /setup/harness-login answers with the run id BEFORE dispatch now
// (internal/api/harnesscred_launch.go), so "resolved" no longer means
// "attachable": handleAttachTicket 409s a non-RUNNING run and a mint failure is
// terminal in AttachTerminal. The pane holds a `starting` phase — with the run
// id, so Cancel kills a sandbox that is still coming up — and polls the run
// until it is RUNNING (or ends).
describe("HarnessLoginPane — the starting phase (P5)", () => {
  beforeEach(() => {
    harnessLoginMock.mockReset().mockResolvedValue("run-123");
    harnessPasteMock.mockReset().mockResolvedValue(undefined);
    lastAttachOutput = undefined;
    lastAttachProps = undefined;
    vi.mocked(runsApiMocked.killRun).mockReset().mockResolvedValue(undefined);
    vi.mocked(runsApiMocked.getRun).mockReset();
    getSetupStatusMock.mockReset();
  });

  async function startAws() {
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));
  }

  it("keeps the run id so Cancel kills a sandbox that is still coming up", async () => {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue({ id: "run-123", state: "PENDING" } as AgentRun);
    await startAws();

    // The waiting copy, not a terminal: attaching to a PENDING run is a 409 the
    // component treats as terminal.
    expect(await screen.findByTestId("login-sandbox-starting")).toBeInTheDocument();
    expect(screen.queryByTestId("fake-terminal")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /cancel/i }));
    expect(runsApiMocked.killRun).toHaveBeenCalledWith("run-123");
  });

  it("polls the run to RUNNING, then mounts the terminal without auto-typing into a self-running sandbox", async () => {
    vi.mocked(runsApiMocked.getRun)
      .mockResolvedValueOnce({ id: "run-123", state: "PENDING" } as AgentRun)
      .mockResolvedValue({ id: "run-123", state: "RUNNING" } as AgentRun);
    await startAws();

    // The second read is a poll tick away (RUN_POLL_MS), not a microtask.
    expect(await screen.findByTestId("fake-terminal", {}, { timeout: 5000 })).toBeInTheDocument();
    // The image starts the chained command itself (deploy/images/aws-sso/signin-pane.sh)
    // and this terminal joins that session, so AttachTerminal's unconditional
    // 900ms type would land on the running login's stdin and run a SECOND
    // wardyn-aws-sso in the same run — already_captured, i.e. a fail marker on a
    // sign-in that worked. The pane's own grace-window fallback replaces it.
    expect(lastAttachProps?.autoRun).toBeUndefined();
    // …but the flow still CARRIES the command, because the fallback types it and
    // cmd/wardyn-aws-sso's TestLoginCommand_UIParity reads this literal.
    expect(loginFlow("aws").cmd).toContain("&& wardyn-aws-sso");
  });

  // R-07: `getRun` failures were swallowed unconditionally ("a blip is not an
  // outcome"), which is right for ONE and wrong for all of them — a daemon
  // restart mid-pull, a pruned run or a 403 after a roster edit left the
  // starting copy on screen forever with nothing but Cancel to end it.
  it("a persistently unreadable run ends the wait, and Cancel still works", async () => {
    vi.mocked(runsApiMocked.getRun).mockRejectedValue(new Error("control plane unreachable"));
    // Drive the poll's own clock rather than waiting out 15 real ticks — 30s of
    // wall time in a unit suite is a cost every CI run pays to learn nothing
    // extra. Installed BEFORE the render: usePoll's setInterval has to be the
    // faked one, and userEvent gets the same clock so its own waits resolve.
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    try {
      render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
      await user.click(screen.getByRole("button", { name: /start login/i }));

      // The first failures are blips: the pane keeps waiting.
      expect(screen.getByTestId("login-sandbox-starting")).toBeInTheDocument();
      expect(screen.queryByRole("alert")).toBeNull();

      // advanceTimersByTimeAsync flushes the microtasks each rejected read
      // queues, which is what usePoll's in-flight guard waits on.
      //
      // FINDING 6: fifteen ticks is no longer an ending. It was called "≈30s"
      // and the reporting estate's cold pull took 131 — so thirty seconds of a
      // daemon being unreachable now says "still trying", and the wait ends on
      // the CLOCK (RUN_POLL_UNREADABLE_AFTER_MS, 150 ticks at this cadence).
      for (let i = 0; i < 15; i++) {
        await act(async () => {
          await vi.advanceTimersByTimeAsync(2000);
        });
      }
      expect(screen.queryByRole("alert")).toBeNull();
      expect(screen.getByTestId("login-sandbox-starting")).toHaveTextContent(LOGIN_SANDBOX_READ_RETRYING);

      for (let i = 15; i < 150; i++) {
        await act(async () => {
          await vi.advanceTimersByTimeAsync(2000);
        });
      }
    } finally {
      vi.useRealTimers();
    }

    const alertBox = screen.getByRole("alert");
    expect(alertBox).toHaveTextContent("stopped being able to read the sign-in sandbox");
    expect(screen.queryByTestId("fake-terminal")).not.toBeInTheDocument();

    // The run id survives the ending, so the sandbox is still the operator's to
    // kill — the wait ended because Wardyn could not READ the run, which is no
    // evidence at all that the sandbox stopped.
    await userEvent.click(screen.getByRole("button", { name: /cancel/i }));
    expect(runsApiMocked.killRun).toHaveBeenCalledWith("run-123");
  });

  // Finding 6, the case the old budget could not express: the reads are FINE,
  // the sandbox just isn't up — a 131-second first pull of the aws-sso image on
  // the reporting estate. Nothing here ever fails, so the failure counter this
  // replaced would have sat at zero forever while the copy claimed the start was
  // ordinary.
  it("a healthy STARTING read past 60s shows the slow-start sentence and never an alert", async () => {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue({ id: "run-123", state: "PENDING" } as AgentRun);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    try {
      render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
      await user.click(screen.getByRole("button", { name: /start login/i }));
      expect(screen.getByTestId("login-sandbox-starting")).toHaveTextContent(LOGIN_SANDBOX_STARTING);

      for (let i = 0; i < 35; i++) {
        await act(async () => {
          await vi.advanceTimersByTimeAsync(2000);
        });
      }
      expect(screen.getByTestId("login-sandbox-starting")).toHaveTextContent(LOGIN_SANDBOX_SLOW_START);
      expect(screen.queryByRole("alert")).toBeNull();

      // Well past the old 30s budget AND past the new 300s one — which does not
      // apply, because nothing is failing.
      for (let i = 35; i < 200; i++) {
        await act(async () => {
          await vi.advanceTimersByTimeAsync(2000);
        });
      }
      expect(screen.queryByRole("alert")).toBeNull();
      expect(screen.getByTestId("login-sandbox-starting")).toHaveTextContent(LOGIN_SANDBOX_SLOW_START);
    } finally {
      vi.useRealTimers();
    }
  });

  // The unreadable clock measures the CURRENT run of failures, not the wait: a
  // sign-in that loses the daemon for four minutes, gets one answer, and loses
  // it again for another four has never been unreadable for five — and ending it
  // on the sum would be the tick counter's mistake with a clock's face on it.
  it("one successful read resets the unreadable clock", async () => {
    let up = false;
    vi.mocked(runsApiMocked.getRun).mockImplementation(async () => {
      if (!up) throw new Error("control plane unreachable");
      up = false; // exactly ONE answer, then dark again
      return { id: "run-123", state: "PENDING" } as AgentRun;
    });
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    try {
      render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
      await user.click(screen.getByRole("button", { name: /start login/i }));

      const advance = async (ticks: number) => {
        for (let i = 0; i < ticks; i++) {
          await act(async () => {
            await vi.advanceTimersByTimeAsync(2000);
          });
        }
      };

      await advance(120); // 240s dark — retrying, not over
      expect(screen.queryByRole("alert")).toBeNull();
      up = true;
      await advance(1); // one answer
      await advance(120); // another 240s dark: 480s total, 240s consecutive
      expect(screen.queryByRole("alert")).toBeNull();

      await advance(35); // now 310s consecutive, and the wait ends
      expect(screen.getByRole("alert")).toHaveTextContent(LOGIN_SANDBOX_UNREADABLE);
    } finally {
      vi.useRealTimers();
    }
  });

  // ── the self-run grace window (finding 4) ─────────────────────────────────
  //
  // The aws-sso image starts the pair in its own tmux session before its prep,
  // and every attach path joins that session. The pane therefore types nothing
  // — UNLESS the sandbox never announced itself, which is what an operator
  // WARDYN_AGENT_IMAGES pin on an older image looks like from here.
  //
  // FAKE TIMERS BEFORE RENDER, always: the pane arms its grace timer the moment
  // it attaches, so timers installed afterwards can never fire it and every
  // "did not type" assertion below would be true for the wrong reason.
  async function attachedOnFakeTimers(): Promise<void> {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue({ id: "run-123", state: "RUNNING" } as AgentRun);
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: /start login/i }));
    await screen.findByTestId("fake-terminal");
  }

  // Each case emits ONE suppressing signal and advances well past the window.
  // The suppression, not the arming, is what is under test — "types after the
  // grace window when an old image is pinned" below is the control that proves
  // the timer fires at all on this same shape.
  for (const [name, chunk] of [
    // The image's own banner, through the exported constant the shell side is
    // pinned against (TestSelfRunBanner_UIParity, cmd/wardyn-aws-sso).
    ["the image announces the self-run", `${SELFRUN_MARKER} — AWS sign-in sandbox.\r\n`],
    // A device code is already on screen: the login is live whatever else the
    // buffer does or does not say.
    ["a device verification URL has already been seen", "https://d-1234567890.awsapps.com/start/#/device?user_code=ABCD-EFGH\r\n"],
    // The refusal line is still printing (no trailing newline yet), so the fail
    // arm has not fired and the phase has not changed — the timer's own
    // failMarker check is the only thing standing between this sandbox and a
    // second sign-in typed over its refusal.
    ["the sandbox is part-way through printing a refusal", "wardyn: aws sso credential rejected: the pinned account"],
  ] as const) {
    it(`does not type when ${name}`, async () => {
      sendTextSpy.mockClear();
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        await attachedOnFakeTimers();
        // NEITHER typing path: AttachTerminal's own unconditional autoRun is
        // what 0.7.4 used, and it fires 900ms after connect with no check at all.
        expect(lastAttachProps?.autoRun).toBeUndefined();
        act(() => lastAttachOutput?.(chunk));
        await act(async () => {
          await vi.advanceTimersByTimeAsync(30_000);
        });
      } finally {
        vi.useRealTimers();
      }
      expect(sendTextSpy).not.toHaveBeenCalled();
    });
  }

  it("types after the grace window when an old image is pinned", async () => {
    sendTextSpy.mockClear();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      await attachedOnFakeTimers();
      // An image that predates the self-run says nothing at all.
      expect(sendTextSpy).not.toHaveBeenCalled();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });
    } finally {
      vi.useRealTimers();
    }
    expect(sendTextSpy).toHaveBeenCalledTimes(1);
    expect(sendTextSpy.mock.calls[0][0]).toBe(loginFlow("aws").cmd + "\r");
  });

  it("a FAILED run shows the run's own failure_hint instead of waiting forever", async () => {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue({
      id: "run-123",
      state: "FAILED",
      failure_hint: "the sandbox image could not be pulled",
    } as AgentRun);
    await startAws();

    const alertBox = await screen.findByRole("alert");
    expect(alertBox).toHaveTextContent("the sandbox image could not be pulled");
    expect(screen.queryByTestId("fake-terminal")).not.toBeInTheDocument();
  });
});

