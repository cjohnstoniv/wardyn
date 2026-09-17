/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// THE LAUNCH CALL'S OWN OUTCOMES, and the blurb the pane puts on screen with
// them (U-8, U-11).
//
// Its own file because harness-login-pane.test.tsx is at the 1000-line gate
// (scripts/check-file-size.sh) — and because this is a different seam: every
// case here ends at the `starting` phase or at the `error` one, with no
// terminal, no capture and no corroboration. What happens AFTER the sandbox is
// up stays in the sibling file.
import * as React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

// The pane reaches AttachTerminal (and through it xterm's stylesheet); no case
// here gets far enough to mount it, but the import itself has to resolve.
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: React.forwardRef(function FakeTerminal(
    _props: { onOutput?: (chunk: string) => void; autoRun?: string },
    ref: React.ForwardedRef<{ sendText: (t: string) => void }>,
  ) {
    React.useImperativeHandle(ref, () => ({ sendText: () => {} }), []);
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
vi.mock("../../../lib/api/runs", () => ({ runs: { killRun: vi.fn(), getRun: vi.fn() } }));
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: vi.fn() } }));

import { HarnessLoginPane } from "./harness-login-pane";
import { AWS_BLURB_MANAGED_OPENING } from "./login-pane-copy";
import { HttpError } from "../../../lib/api/core";
import { runs as runsApiMocked } from "../../../lib/api/runs";
import type { AgentRun } from "../../../lib/types";

// U-11 (W6 blind lens) — the no-credential member preview offers "Sign in to
// AWS" (Getting Started renders the CTA off a not_configured state) and the
// launch is then refused with a deterministic 409 (harnesscred_launch.go). The
// pane answered with "Try again", which earns the identical refusal: the reader
// has to leave the preview, and nothing on that screen said so.
describe("a REFUSED launch offers no retry (U-11)", () => {
  beforeEach(() => {
    harnessLoginMock.mockReset();
    vi.mocked(runsApiMocked.killRun).mockReset().mockResolvedValue(undefined);
    vi.mocked(runsApiMocked.getRun).mockReset();
  });

  it("a 409 shows the refusal and NO Try again", async () => {
    harnessLoginMock.mockRejectedValue(new HttpError(409, "sign-in is refused in the member preview"));
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent("sign-in is refused in the member preview");
    expect(screen.queryByRole("button", { name: /try again/i })).not.toBeInTheDocument();
    // …and the way out is still there.
    expect(screen.getByRole("button", { name: /cancel/i })).toBeInTheDocument();
  });

  // The bound: every OTHER failure is a failure, and retrying one is the right
  // move — a 500, a dropped socket, a bad start URL.
  it("a 500 keeps Try again", async () => {
    harnessLoginMock.mockRejectedValue(new HttpError(500, "control plane unreachable"));
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent("control plane unreachable");
    expect(screen.getByRole("button", { name: /try again/i })).toBeInTheDocument();
  });

  // …and a retry that then WORKS clears the refusal rather than carrying it.
  it("Try again after a 500 relaunches", async () => {
    harnessLoginMock.mockRejectedValueOnce(new HttpError(500, "control plane unreachable"));
    harnessLoginMock.mockResolvedValue("run-123");
    vi.mocked(runsApiMocked.getRun).mockResolvedValue({ id: "run-123", state: "PENDING" } as AgentRun);
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));
    await screen.findByRole("alert");
    await userEvent.click(screen.getByRole("button", { name: /try again/i }));
    expect(await screen.findByTestId("login-sandbox-starting")).toBeInTheDocument();
  });
});

// U-8 (W6 blind lens) — the aws blurb asked the reader to give Wardyn their
// organization's access portal URL, under a managed row where there is no field,
// the server ignores a supplied one, and the intro one line above has just said
// there is nothing to enter.
describe("the aws blurb under a managed access portal (U-8)", () => {
  async function blurbAfterStart(startURLManaged: boolean) {
    harnessLoginMock.mockReset().mockResolvedValue("run-123");
    vi.mocked(runsApiMocked.getRun)
      .mockReset()
      .mockResolvedValue({ id: "run-123", state: "PENDING" } as AgentRun);
    const { container } = render(
      <HarnessLoginPane
        provider="aws"
        startURLManaged={startURLManaged}
        onDone={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    if (!startURLManaged) {
      await userEvent.type(screen.getByLabelText("AWS access portal start URL"), "https://acme.awsapps.com/start");
    }
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));
    await screen.findByTestId("login-sandbox-starting");
    return container.textContent ?? "";
  }

  it("a managed row says the portal is already set, never asks for it", async () => {
    const text = await blurbAfterStart(true);
    expect(text).toContain(AWS_BLURB_MANAGED_OPENING);
    expect(text).not.toContain("Give Wardyn your organization");
    // The rest of the blurb is unchanged — same sandbox, same config, same command.
    expect(text).toContain("holding just that URL and the configured SSO region");
  });

  it("the ordinary Settings flow still asks for it", async () => {
    const text = await blurbAfterStart(false);
    expect(text).toContain("Give Wardyn your organization");
    expect(text).not.toContain(AWS_BLURB_MANAGED_OPENING);
  });
});
