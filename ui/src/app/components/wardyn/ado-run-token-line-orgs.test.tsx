/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import type { SCMAccessPAT } from "../../lib/types/ado-pat";
import { AdoOwnTokenNote, ownTokenNoteOrgs } from "./ado-run-token-line";

const own = (org: string, state: string, cause?: string): SCMAccessPAT => ({ state, cause, token_mode: "own_pat", org: `https://dev.azure.com/${org}` });

// #2146: the launch note names the organisations that still need a token.
describe("AdoOwnTokenNote", () => {
  it("names the organisations missing a token, and not a live one", () => {
    render(<AdoOwnTokenNote rows={[own("acme", "live"), own("beta", "not_configured")]} />);
    expect(screen.getByTestId("ado-own-token-note")).toHaveTextContent("Add your Azure DevOps token for beta first.");
  });

  it("names several: acme and beta", () => {
    render(<AdoOwnTokenNote rows={[own("acme", "expired_signin", "token_expired"), own("beta", "not_configured")]} />);
    expect(screen.getByTestId("ado-own-token-note")).toHaveTextContent("Add your Azure DevOps token for acme and beta first.");
  });

  it("says nothing when every organisation has a token", () => {
    render(<AdoOwnTokenNote rows={[own("acme", "live"), own("beta", "expiring")]} />);
    expect(screen.queryByTestId("ado-own-token-note")).not.toBeInTheDocument();
  });

  it("says nothing for a single organisation: that page is unchanged", () => {
    expect(ownTokenNoteOrgs([own("beta", "not_configured")])).toEqual([]);
    expect(ownTokenNoteOrgs(undefined)).toEqual([]);
  });
});
