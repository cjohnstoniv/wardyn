/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SetupStatus } from "../../../lib/types";
import { ADO } from "../../../lib/ado-entra-copy";

const adoConnectMock = vi.fn();
let adoBlockedUrl: string | null = null;
vi.mock("../../../lib/hooks/use-ado-connect", () => ({
  useAdoConnect: () => ({ connecting: false, connect: adoConnectMock, connectFallback: adoConnectMock, blockedUrl: adoBlockedUrl }),
}));

import { AdoConnectionCard } from "./ado-connection";

function status(scm_access?: SetupStatus["scm_access"]): SetupStatus {
  return { ready: true, checks: [], auth: { mode: "local" }, runner: { driver: "docker", confinement_classes: [] }, providers: [], secrets: { present: [] }, age_key: { durable: true }, has_runs: false, scm_access } as unknown as SetupStatus;
}

describe("AdoConnectionCard — the connected panel's Settings home (#386, Q9)", () => {
  beforeEach(() => {
    adoConnectMock.mockReset();
    adoBlockedUrl = null;
  });

  it("renders nothing with no Azure DevOps row configured", () => {
    render(<AdoConnectionCard status={status(undefined)} onChanged={vi.fn()} />);
    expect(screen.queryByText("Azure DevOps")).not.toBeInTheDocument();
  });

  it("live: the org, how it connected, and the renewal note — no button", () => {
    render(
      <AdoConnectionCard
        status={status({ state: "live", source: "org", org: "https://dev.azure.com/contoso" })}
        onChanged={vi.fn()}
      />,
    );
    expect(screen.getByText(ADO.ACCESS_LIVE_ORG)).toBeInTheDocument();
    expect(screen.getByText("https://dev.azure.com/contoso")).toBeInTheDocument();
    expect(screen.getByText(new RegExp(ADO.PANEL_HOW_ORG))).toBeInTheDocument();
    expect(screen.getByText(new RegExp(ADO.PANEL_ENDS_RENEWED))).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: ADO.CONNECT_ADO })).not.toBeInTheDocument();
  });

  // Review finding F3: a SHARED row's `live` (no source) must render
  // ACCESS_SHARED_NOTE, never a per-person claim like "Your Wardyn sign-in"
  // or "Renewed while you keep using it".
  it("live on a shared row (no source): ACCESS_SHARED_NOTE, never the per-person panel", () => {
    render(<AdoConnectionCard status={status({ state: "live", org: "https://dev.azure.com/contoso" })} onChanged={vi.fn()} />);
    expect(screen.getByText(ADO.ACCESS_SHARED_LIVE)).toBeInTheDocument();
    expect(screen.getByText(ADO.ACCESS_SHARED_NOTE)).toBeInTheDocument();
    expect(screen.queryByText(new RegExp(ADO.PANEL_HOW_ORG))).not.toBeInTheDocument();
    expect(screen.queryByText(new RegExp(ADO.PANEL_ENDS_RENEWED))).not.toBeInTheDocument();
  });

  it("not_configured: the cause and the connect button, which reloads status on a real connection", async () => {
    adoConnectMock.mockResolvedValueOnce(true);
    const onChanged = vi.fn();
    render(<AdoConnectionCard status={status({ state: "not_configured", cause: "row_is_newer" })} onChanged={onChanged} />);
    expect(screen.getByText(ADO.CAUSE_ROW_IS_NEWER)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: ADO.CONNECT_ADO }));
    expect(adoConnectMock).toHaveBeenCalledTimes(1);
    expect(onChanged).toHaveBeenCalledTimes(1);
  });

  it("shared_expired: the admin-facing action line, no button", () => {
    render(<AdoConnectionCard status={status({ state: "shared_expired" })} onChanged={vi.fn()} />);
    expect(screen.getByText(ADO.ACCESS_SHARED_EXPIRED_ACTION)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: ADO.CONNECT_ADO })).not.toBeInTheDocument();
  });
});

// Review finding F9 — a blocked popup.
describe("AdoConnectionCard — a blocked popup (F9)", () => {
  beforeEach(() => {
    adoConnectMock.mockReset();
    adoBlockedUrl = "/api/v1/scm/azure-devops/signin";
  });

  it("shows the canon sentence and a plain fallback link to the sign-in URL, alongside the button", () => {
    render(<AdoConnectionCard status={status({ state: "not_configured", cause: "row_is_newer" })} onChanged={vi.fn()} />);
    expect(screen.getByText(ADO.CONNECT_POPUP_BLOCKED)).toBeInTheDocument();
    const link = screen.getByRole("link", { name: ADO.CONNECT_ADO });
    expect(link).toHaveAttribute("href", "/api/v1/scm/azure-devops/signin");
    expect(link).toHaveAttribute("target", "_blank");
  });

  // Review follow-up N1: clicking the fallback link starts the SAME poll
  // (connectFallback), so the card reloads status on a real connection.
  it("N1: clicking the fallback link starts the poll and reloads status once connected", async () => {
    adoConnectMock.mockResolvedValueOnce(true);
    const onChanged = vi.fn();
    render(<AdoConnectionCard status={status({ state: "not_configured", cause: "row_is_newer" })} onChanged={onChanged} />);
    await userEvent.click(screen.getByRole("link", { name: ADO.CONNECT_ADO }));
    expect(adoConnectMock).toHaveBeenCalledTimes(1);
    expect(onChanged).toHaveBeenCalledTimes(1);
  });
});
