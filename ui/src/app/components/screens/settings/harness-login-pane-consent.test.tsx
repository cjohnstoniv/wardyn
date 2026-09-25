/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Split from harness-login-pane.test.tsx (#195): that file was over the
// 800-line test gate. The consent-gate describe (and the terminal/harness/
// setup/audit mocks it shares with the sibling's starting-phase describe) live
// here; isLikelyStartUrl, loginFlow and serverConfirmsCapture stay there.
import * as React from "react";
import { act } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import {
  SANDBOX_REFUSAL_LEAD_IN,
  HarnessLoginPane,
  CAPTURE_NOT_CORROBORATED,
} from "./harness-login-pane";
import { SIGNIN_PROGRESS } from "./login-pane-copy";
import { CAPTURE_POST_RUN_GRACE_MS } from "./capture-confirm";
import { runs as runsApiMocked } from "../../../lib/api/runs";
import type { SetupStatus } from "../../../lib/types";
import { makeRun } from "../../../../test/factories";

// A realistic setup-token body: sk-ant-oat<2 digits>-<long url-safe blob>.
const TOKEN = "sk-ant-oat01-" + "A".repeat(60) + "-_" + "b3".repeat(10);


// The consent gate.
//
// Owner report, verbatim: "you see a dialog then all of a sudden terminal then
// all of a sudden a popup asking for auth. We should alert the user before
// this happens what to expect and what's required from them." Nothing
// launches the sandbox except Start login.

// lastAttachOutput captures the onOutput callback the pane hands AttachTerminal
// on its most recent render, so a test can feed it PTY chunks directly — the
// only way to exercise handleOutput's marker-watching without a real xterm
// (which does not render in jsdom).
let lastAttachOutput: ((chunk: string) => void) | undefined;
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
    vi.mocked(runsApiMocked.killRun).mockReset().mockResolvedValue(undefined);
    // P5: the pane no longer mounts the terminal on the POST's own resolve —
    // it polls the run to RUNNING first (a 200 now precedes dispatch). Every
    // case below that wants a terminal gets a run that is already up; the
    // starting/failed shapes drive this mock themselves.
    vi.mocked(runsApiMocked.getRun)
      .mockReset()
      .mockResolvedValue(makeRun({ id: "run-123", state: "RUNNING" }));
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
    expect(screen.getByText(/verification page/i)).toHaveTextContent(`“${SIGNIN_PROGRESS.OPEN("AWS")}” opens it`);
  });

  // #628: nothing opens on Start any more, so the intro and blurb promise the
  // Open button, never a tab that "opens" by itself.
  it("the Claude intro and blurb name the Open button, never a tab that opens on its own", async () => {
    harnessLoginMock.mockReturnValue(new Promise<string>(() => {}));
    render(<HarnessLoginPane provider="anthropic" onDone={vi.fn()} onCancel={vi.fn()} />);
    const intro = screen.getByTestId("login-intro");
    expect(intro).toHaveTextContent(`“${SIGNIN_PROGRESS.OPEN("Claude")}” opens it in a new tab`);
    expect(intro).not.toHaveTextContent(/A new tab opens/);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));
    const pane = screen.getByTestId("harness-login-pane");
    expect(pane).toHaveTextContent(`open it with “${SIGNIN_PROGRESS.OPEN("Claude")}”`);
    expect(pane).not.toHaveTextContent(/opens the Claude login page in a new tab/);
  });

  // Under a per_user agent row the server signs in against the ROW's stored
  // sso_start_url and IGNORES whatever is typed here, so asking is a field
  // that cannot take effect — and every member would have to hunt down a URL
  // their admin already entered.
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
  // NEVER prints doneMarker in that case. Without this latch, the pane would
  // sit on "waiting" forever with no explanation, the operator's only signal
  // a terminal that stopped scrolling.
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

    // The run is already killed by this point, so the terminal's socket just
    // closes — but the SCROLLBACK (device-code chatter, the portal's reply,
    // the helper's own preceding lines) stays on screen beside the alert
    // rather than vanishing with it, since the one extracted sentence is
    // otherwise the operator's only artifact.
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
      expect(screen.queryByTestId("signin-ready")).not.toBeInTheDocument();
      expect(runsApiMocked.killRun).toHaveBeenCalledWith("run-123");
      expect(onDone).not.toHaveBeenCalled();
    });

    it("never mistakes the fail marker's own line for a success", async () => {
      await attachAwsRun();
      await act(async () => lastAttachOutput?.("wardyn: aws sso credential rejected: portal timeout.\n"));
      await screen.findByRole("alert");
      expect(screen.queryByText(/session captured/i)).not.toBeInTheDocument();
    });

    // Owner field report, end to end: the opened tab's URL is the artifact
    // the owner actually saw junk in. Pin it at the seam they hit — a
    // colorized device URL through the same onOutput callback the real PTY
    // drives — not just at extractDeviceVerificationUrl's own unit tests.
    it("the AWS verification link and its code are clean even when the PTY colorizes the device URL", async () => {
      await attachAwsRun();
      const clean = "https://d-1234567890.awsapps.com/start/#/device?user_code=ABCD-EFGH";
      await act(async () => lastAttachOutput?.(`\x1b[32m${clean}\x1b[0m\r\n`));
      const ready = await screen.findByTestId("signin-ready");
      expect(ready).toHaveTextContent(clean);
      expect(screen.getByTestId("signin-device-code")).toHaveTextContent(/^ABCD-EFGH$/);
    });
  });

  // `saving` with no autoCapture is TWO different states. On a helper
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
      vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({ id: "run-123", state: "COMPLETED" }));
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

    // The status read can ANSWER and simply not show this run's capture yet —
    // a lagging replica, or the supersede's kill landing between the 204 and
    // the read. The write can precede the read without being VISIBLE to it,
    // so refusing on that first read tells a person whose sign-in actually
    // worked to do it again.
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
      // on a terminal that has quietly stopped scrolling. A screen-reader user
      // hears it too — the whole point of narrating a silent round trip is
      // lost in a note no live region announces.
      expect(screen.getByTestId("capture-verifying-note")).toBeInTheDocument();
      expect(screen.getByTestId("capture-verifying-note")).toHaveAttribute("role", "status");

      await act(async () => {
        release({ harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }] } as unknown as SetupStatus);
      });
    });
  });
});
