/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's token list (mock states 6 and 7): one line per token, oldest
// first, under "Azure DevOps" in the Credentials widget.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import type { ADORunToken } from "../../../lib/types/ado-pat";
import type { AuditEvent } from "../../../lib/types";
import { ADO_PAT } from "../../../lib/ado-pat-copy";
import { AdoRunTokens } from "./ado-run-tokens";
import { CredentialsWidget } from "./widgets/credentials";

const runTokensMock = vi.fn();
vi.mock("../../../lib/api/ado-pat", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/ado-pat")>("../../../lib/api/ado-pat");
  return { ...actual, adoPat: { ...actual.adoPat, runTokens: (id: string) => runTokensMock(id) } };
});

const at = (h: number, m: number) => new Date(2000, 8, 29, h, m).toISOString();
const tok = (over: Partial<ADORunToken> = {}): ADORunToken => ({ created_at: at(9, 2), valid_to: at(17, 2), scope: ["vso.code", "vso.project"], ...over });

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
    await draw([tok({ created_at: at(15, 2), valid_to: at(23, 2) }), tok({ revoke_reason: "expired", revoked_at: at(17, 2) })]);
    const old = await screen.findByText("Azure DevOps token: created 09:02 · expires 17:02 (renewed)");
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

  it("6f: a revoke that failed says when the token expires and where to revoke it by hand", async () => {
    await draw([tok({ revoke_failed: true, revoked_at: at(9, 41), revoke_reason: "run_end" })]);
    expect(await screen.findByText(ADO_PAT.RUN_REVOKE_FAILED("17:02"))).toBeInTheDocument();
  });

  it("7: a token a wider one replaced reads (access added), with no separate 'Access added' line", async () => {
    await draw([tok({ created_at: at(9, 20), valid_to: at(17, 20), scope: ["vso.code", "vso.project", "vso.code_write"] }), tok({})]);
    expect(await screen.findByText("Azure DevOps token: created 09:02 · expires 17:02 (access added)")).toBeInTheDocument();
    expect(screen.getByText("Azure DevOps token: created 09:20 · expires 17:20")).toBeInTheDocument();
    expect(screen.queryByText(/^Access added/)).not.toBeInTheDocument();
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

// T17: a run the per-person lane declined says why under "Azure DevOps", in the
// server's own sentence; an ordinary run draws nothing of it.
describe("CredentialsWidget — the Azure DevOps decline", () => {
  const detail =
    "This run was not given an Azure DevOps credential: no Azure DevOps provider admits its repository.";
  const skip = {
    id: "ev-1",
    time: at(9, 0),
    run_id: "run-1",
    actor_type: "system",
    actor: "wardynd",
    action: "run.ado_lane.skip",
    target: "run-1",
    outcome: "denied",
    data: { reason: "row_not_admitted", detail },
  } as AuditEvent;

  it("renders the decline's detail under the Azure DevOps heading", async () => {
    runTokensMock.mockResolvedValue([]);
    render(<CredentialsWidget grants={[]} audit={[skip]} />);
    const block = screen.getByTestId("ado-lane-declined");
    expect(block).toHaveTextContent(ADO_PAT.RUN_TOKEN_TITLE);
    expect(screen.getByRole("alert")).toHaveTextContent(detail);
  });

  it("renders the approved sentence from the reason code, and the row's detail for a code it does not know", async () => {
    runTokensMock.mockResolvedValue([]);
    const known = { ...skip, data: { reason: "no_person", detail: "older server wording" } } as AuditEvent;
    const { unmount } = render(<CredentialsWidget grants={[]} audit={[known]} />);
    expect(screen.getByRole("alert")).toHaveTextContent(
      /^This run was not given an Azure DevOps credential: no signed-in person started it\.$/,
    );
    unmount();
    const unknown = { ...skip, data: { reason: "a_new_code", detail: "newer server wording" } } as AuditEvent;
    render(<CredentialsWidget grants={[]} audit={[unknown]} />);
    expect(screen.getByRole("alert")).toHaveTextContent(/^newer server wording$/);
  });

  it("renders nothing of it for an ordinary run", () => {
    render(<CredentialsWidget grants={[]} audit={[]} />);
    expect(screen.queryByTestId("ado-lane-declined")).not.toBeInTheDocument();
  });
});
