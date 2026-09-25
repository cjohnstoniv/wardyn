/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pane behind a model provider's door (#544): it launches and stores
// through /model-providers/{id}/sign-in, never /setup/harness-*, starts at once
// (packet E draws no consent step), and corroborates an AWS capture by THIS
// run's audit row, since the provider's capture never shows in the harness rows.
import * as React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";

let lastAttachOutput: ((chunk: string) => void) | undefined;
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: React.forwardRef(function FakeTerminal(
    props: { onOutput?: (chunk: string) => void },
    ref: React.ForwardedRef<{ sendText: (t: string) => void }>,
  ) {
    lastAttachOutput = props.onOutput;
    React.useImperativeHandle(ref, () => ({ sendText: () => {} }), []);
    return <div data-testid="fake-terminal" />;
  }),
}));
const harnessLogin = vi.fn();
const harnessCredentialPaste = vi.fn();
vi.mock("../../../lib/api/harness-auth", () => ({
  harnessAuth: {
    harnessLogin: (...a: unknown[]) => harnessLogin(...a),
    harnessCredentialPaste: (...a: unknown[]) => harnessCredentialPaste(...a),
  },
}));
const startSignIn = vi.fn();
const captureSignIn = vi.fn();
vi.mock("../../../lib/api/model-provider-signin", () => ({
  modelProviderSignIn: {
    startSignIn: (...a: unknown[]) => startSignIn(...a),
    captureSignIn: (...a: unknown[]) => captureSignIn(...a),
  },
}));
vi.mock("../../../lib/api/runs", () => ({ runs: { killRun: vi.fn(), getRun: vi.fn() } }));
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: vi.fn() } }));
vi.mock("../../../lib/api/audit", () => ({ audit: { listAudit: vi.fn() } }));

import { HarnessLoginPane } from "./harness-login-pane";
import { confirmCaptureWithServer, serverConfirmsProviderCapture } from "./capture-confirm";
import { runs as runsApi } from "../../../lib/api/runs";
import { setup as setupApi } from "../../../lib/api/setup";
import { audit as auditApi } from "../../../lib/api/audit";
import { makeRun } from "../../../../test/factories";
import { MODEL_PROVIDERS, providerStatus } from "../../../lib/test-fixtures";
import type { AuditEvent } from "../../../lib/types";

const RUN = "3f1b7c26-0000-4000-8000-000000000001";
const TOKEN = "sk-ant-oat01-" + "A".repeat(60);

beforeEach(() => {
  lastAttachOutput = undefined;
  for (const m of [harnessLogin, harnessCredentialPaste, startSignIn, captureSignIn]) m.mockReset();
  startSignIn.mockResolvedValue({ runId: RUN, state: "PENDING" });
  captureSignIn.mockResolvedValue(undefined);
  vi.mocked(runsApi.killRun).mockReset().mockResolvedValue(undefined);
  vi.mocked(runsApi.getRun).mockReset().mockResolvedValue(makeRun({ id: RUN, state: "RUNNING" }));
  vi.mocked(auditApi.listAudit).mockReset().mockResolvedValue([]);
  vi.mocked(setupApi.getSetupStatus).mockReset();
});

describe("a provider door's pane", () => {
  it("starts at once through the provider's own door, once, and never calls /setup/harness-login", async () => {
    render(
      <React.StrictMode>
        <HarnessLoginPane provider="aws" modelProvider="bedrock-prod" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />
      </React.StrictMode>,
    );
    await screen.findByTestId("fake-terminal");
    expect(startSignIn).toHaveBeenCalledTimes(1);
    expect(startSignIn).toHaveBeenCalledWith("bedrock-prod");
    expect(harnessLogin).not.toHaveBeenCalled();
    // No consent step and no pane title: the door frames it (packet E).
    expect(screen.queryByRole("button", { name: /start login/i })).toBeNull();
    expect(screen.queryByText(/via container login/)).toBeNull();
  });

  it("a Claude sign-in's token is stored against the provider and this run", async () => {
    const onDone = vi.fn();
    render(<HarnessLoginPane provider="anthropic" modelProvider="claude-sub" onDone={onDone} onCancel={vi.fn()} />);
    await screen.findByTestId("fake-terminal");
    await act(async () => lastAttachOutput?.(`Your OAuth token: ${TOKEN}\r\n`));
    await waitFor(() => expect(onDone).toHaveBeenCalledTimes(1));
    expect(captureSignIn).toHaveBeenCalledWith("claude-sub", RUN, TOKEN);
    expect(harnessCredentialPaste).not.toHaveBeenCalled();
  });
});

describe("serverConfirmsProviderCapture — this run's audit row, and the provider's own state", () => {
  const status = (state: string) => providerStatus([{ provider: MODEL_PROVIDERS.bedrock, state }]);

  it("needs both: the audit row proves THIS run, the state proves it is there", () => {
    expect(serverConfirmsProviderCapture(status("live"), "bedrock-prod", true)).toBe(true);
    expect(serverConfirmsProviderCapture(status("expiring"), "bedrock-prod", true)).toBe(true);
    // A live session from an earlier sign-in is not this sign-in (R-1).
    expect(serverConfirmsProviderCapture(status("live"), "bedrock-prod", false)).toBe(false);
    expect(serverConfirmsProviderCapture(status("not_configured"), "bedrock-prod", true)).toBe(false);
    expect(serverConfirmsProviderCapture(status("live"), "other", true)).toBe(false);
  });

  it("the round trip reads this run's audit row", async () => {
    vi.mocked(setupApi.getSetupStatus).mockResolvedValue(status("live"));
    vi.mocked(auditApi.listAudit).mockResolvedValue([{ id: 1 } as unknown as AuditEvent]);
    await expect(confirmCaptureWithServer("aws", RUN, "bedrock-prod")).resolves.toEqual({
      confirmed: true,
      unreachable: false,
    });
    expect(auditApi.listAudit).toHaveBeenCalledWith(RUN, "harness.credential.captured");
  });

  it("a forged marker — no audit row — is not confirmed", async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(setupApi.getSetupStatus).mockResolvedValue(status("live"));
      const p = confirmCaptureWithServer("aws", RUN, "bedrock-prod");
      await vi.runAllTimersAsync();
      await expect(p).resolves.toEqual({ confirmed: false, unreachable: false });
    } finally {
      vi.useRealTimers();
    }
  });
});
