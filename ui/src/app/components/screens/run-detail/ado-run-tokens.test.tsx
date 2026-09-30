/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's token list (mock states 6 and 7): one line per token, oldest
// first, under "Azure DevOps" in the Credentials widget.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import type { ADORunToken } from "../../../lib/types/ado-pat";
import { ADO_PAT } from "../../../lib/ado-pat-copy";
import { AdoRunTokens } from "./ado-run-tokens";
import { CredentialsWidget } from "./widgets/credentials";

const runTokensMock = vi.fn();
vi.mock("../../../lib/api/ado-pat", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/ado-pat")>("../../../lib/api/ado-pat");
  return { ...actual, adoPat: { ...actual.adoPat, runTokens: (id: string) => runTokensMock(id) } };
});

const at = (h: number, m: number) => new Date(2026, 8, 29, h, m).toISOString();
const tok = (over: Partial<ADORunToken> = {}): ADORunToken => ({ created_at: at(9, 2), valid_to: at(17, 2), ...over });

beforeEach(() => {
  runTokensMock.mockReset();
});

async function draw(tokens: ADORunToken[], paused = false) {
  runTokensMock.mockResolvedValue(tokens);
  render(<AdoRunTokens runId="run-1" paused={paused} live={false} />);
  await waitFor(() => expect(runTokensMock).toHaveBeenCalledWith("run-1"));
}

describe("AdoRunTokens", () => {
  it("6a: an active token is one line under the Azure DevOps heading", async () => {
    await draw([tok()]);
    expect(await screen.findByText("Azure DevOps token: created 09:02 · expires 17:02")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: ADO_PAT.RUN_TOKEN_TITLE })).toBeInTheDocument();
  });

  it("6b: a renewed run lists both tokens, oldest first, the old one muted", async () => {
    await draw([tok({ created_at: at(15, 2), valid_to: at(23, 2) }), tok({ revoked_at: at(15, 2), revoke_reason: "renewal" })]);
    const old = await screen.findByText("Azure DevOps token: created 09:02 · expires 17:02 · revoked 15:02 (renewed)");
    const current = screen.getByText("Azure DevOps token: created 15:02 · expires 23:02");
    expect(old).toHaveClass("text-muted-foreground");
    expect(current).not.toHaveClass("text-muted-foreground");
    expect(old.compareDocumentPosition(current) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("6c: a paused run says its token was revoked and a new one comes on resume", async () => {
    await draw([tok({ revoked_at: at(11, 30), revoke_reason: "pause" })], true);
    expect(await screen.findByText("Azure DevOps token: created 09:02 · expires 17:02 · revoked 11:30 (paused)")).toBeInTheDocument();
    expect(screen.getByText(ADO_PAT.RUN_PAUSED)).toBeInTheDocument();
  });

  it("6d: a finished run's token shows when and why it was revoked", async () => {
    await draw([tok({ revoked_at: at(9, 41), revoke_reason: "run_end" })]);
    expect(await screen.findByText("Azure DevOps token: created 09:02 · expires 17:02 · revoked 09:41 (run ended)")).toBeInTheDocument();
    expect(screen.queryByText(ADO_PAT.RUN_PAUSED)).not.toBeInTheDocument();
  });

  it("6e: a failed renewal says when the token stops working and to sign in again", async () => {
    await draw([tok({ renewal_failed: true })]);
    expect(await screen.findByText(ADO_PAT.RUN_RENEWAL_FAILED("17:02"))).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(
      "Wardyn couldn't renew this run's token, so it stops working at 17:02. Sign in to Azure DevOps again to keep this run going.",
    );
  });

  it("6f: a revoke that failed says when the token expires and where to revoke it by hand", async () => {
    await draw([tok({ revoke_failed: true, revoked_at: at(9, 41), revoke_reason: "run_end" })]);
    expect(await screen.findByText(ADO_PAT.RUN_REVOKE_FAILED("17:02"))).toBeInTheDocument();
  });

  it("7: a widening shows the old token revoked (access added), the new one, and the added access", async () => {
    await draw([
      tok({ created_at: at(9, 20), valid_to: at(17, 20), added_capabilities: ["pr"] }),
      tok({ revoked_at: at(9, 21), revoke_reason: "widen" }),
    ]);
    expect(await screen.findByText("Azure DevOps token: created 09:02 · expires 17:02 · revoked 09:21 (access added)")).toBeInTheDocument();
    expect(screen.getByText("Azure DevOps token: created 09:20 · expires 17:20")).toBeInTheDocument();
    expect(screen.getByText(/^Access added 09:20: .+ \(new token\)$/)).toBeInTheDocument();
  });

  it("draws nothing for a run that holds no token", async () => {
    runTokensMock.mockResolvedValue([]);
    const { container } = render(<AdoRunTokens runId="run-1" paused={false} live={false} />);
    await waitFor(() => expect(runTokensMock).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });

  it("draws nothing when the read fails", async () => {
    runTokensMock.mockImplementation(() => Promise.reject(new Error("no such route")));
    const { container } = render(<AdoRunTokens runId="run-1" paused={false} live={false} />);
    await waitFor(() => expect(runTokensMock).toHaveBeenCalled());
    await act(async () => {});
    expect(container).toBeEmptyDOMElement();
  });

  it("lives in the Credentials widget beneath the grants", async () => {
    runTokensMock.mockResolvedValue([tok()]);
    render(<CredentialsWidget grants={[]} audit={[]} ado={{ runId: "run-1", paused: false, live: false }} />);
    expect(await screen.findByTestId("ado-run-tokens")).toBeInTheDocument();
  });
});
