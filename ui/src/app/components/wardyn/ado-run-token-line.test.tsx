/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's Azure DevOps lines for a row that creates a token per run (mock
// state 5, 8a): what the run's token carries, and the note when a launch would
// be refused.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SCMAccessPAT } from "../../lib/types/ado-pat";
import { ADO_PAT } from "../../lib/ado-pat-copy";
import { adoCapName } from "../../lib/ado-access-copy";
import { AdoLaunchNote, AdoRunTokenLine } from "./ado-run-token-line";

describe("AdoRunTokenLine", () => {
  it("names the policy's capabilities in one sentence, and says the token is revoked when the run ends", () => {
    render(<AdoRunTokenLine policyCaps={["code_read", "code_write", "pr"]} />);
    const line = screen.getByTestId("ado-run-token-line");
    const names = ["code_read", "code_write", "pr"].map(adoCapName).join(", ");
    expect(line).toHaveTextContent(`Azure DevOps: a token for this run with ${names}. It's revoked when the run ends.`);
    expect(line.querySelector("b")).toHaveTextContent(names);
  });

  it("falls back to the row's default profile when the policy names none", () => {
    render(<AdoRunTokenLine policyCaps={undefined} defaults={["code_read"]} />);
    expect(screen.getByTestId("ado-run-token-line")).toHaveTextContent(`with ${adoCapName("code_read")}.`);
  });

  it("says nothing when neither names any access", () => {
    render(<AdoRunTokenLine policyCaps={[]} />);
    expect(screen.queryByTestId("ado-run-token-line")).not.toBeInTheDocument();
  });
});

describe("AdoLaunchNote", () => {
  const access = (over: Partial<SCMAccessPAT>) => ({ token_mode: "minted_pat", state: "not_configured", ...over }) as SCMAccessPAT;

  it("5: not connected says to connect once first, and Connect Azure DevOps starts the connection", async () => {
    const onConnect = vi.fn();
    render(<AdoLaunchNote access={access({})} connecting={false} onConnect={onConnect} />);
    expect(screen.getByText(ADO_PAT.LAUNCH_NOT_CONNECTED)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.MEMBER_CONNECT }));
    expect(onConnect).toHaveBeenCalledTimes(1);
  });

  it("a connection that ended asks to connect again the same way", () => {
    render(<AdoLaunchNote access={access({ state: "expired_signin" })} connecting={false} onConnect={vi.fn()} />);
    expect(screen.getByText(ADO_PAT.LAUNCH_NOT_CONNECTED)).toBeInTheDocument();
  });

  it("disables Connect while the connection is in progress", () => {
    render(<AdoLaunchNote access={access({})} connecting onConnect={vi.fn()} />);
    expect(screen.getByRole("button", { name: ADO_PAT.MEMBER_CONNECT })).toBeDisabled();
  });

  it("8a: blocked by the organisation says the launch is refused and who can fix it, with no button", () => {
    render(<AdoLaunchNote access={access({ state: "expired_signin", cause: "blocked" })} connecting={false} onConnect={vi.fn()} />);
    expect(screen.getByText(ADO_PAT.LAUNCH_POLICY_REFUSED)).toBeInTheDocument();
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });

  it("a launch refused because the organisation blocks tokens says so, whatever the answer was", () => {
    render(<AdoLaunchNote access={access({ state: "live" })} refusal="blocked" connecting={false} onConnect={vi.fn()} />);
    expect(screen.getByText(ADO_PAT.LAUNCH_POLICY_REFUSED)).toBeInTheDocument();
  });

  it("a launch refused for want of a usable sign-in asks to connect again, even before the answer is read", async () => {
    const onConnect = vi.fn();
    render(<AdoLaunchNote access={undefined} refusal="connect" connecting={false} onConnect={onConnect} />);
    expect(screen.getByText(ADO_PAT.LAUNCH_NOT_CONNECTED)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.MEMBER_CONNECT }));
    expect(onConnect).toHaveBeenCalledTimes(1);
  });

  it("a row the console cannot redeem is the admin's to fix: no Connect to press", () => {
    const { container } = render(
      <AdoLaunchNote access={access({ state: "expired_signin", cause: "ado_pat_needs_console_app" })} connecting={false} onConnect={vi.fn()} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("says nothing when connected, on the Entra sign-in lane, or with no answer", () => {
    const { container, rerender } = render(<AdoLaunchNote access={access({ state: "live" })} connecting={false} onConnect={vi.fn()} />);
    expect(container).toBeEmptyDOMElement();
    rerender(<AdoLaunchNote access={access({ token_mode: "bearer" })} connecting={false} onConnect={vi.fn()} />);
    expect(container).toBeEmptyDOMElement();
    rerender(<AdoLaunchNote access={undefined} connecting={false} onConnect={vi.fn()} />);
    expect(container).toBeEmptyDOMElement();
  });
});
