/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #659 Q2 — the Azure DevOps sign-in home banner: reads the redirect's own
// query params once, renders the matching sentence, clears the query string,
// and (identity_binding only) sends the retry through prompt=select_account.
import { afterEach, describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { AdoSignInBanner } from "./ado-signin-banner";
import { ADO_SIGNIN } from "./copy/ado-signin-error-copy";

function setURL(pathAndQuery: string) {
  window.history.pushState({}, "", pathAndQuery);
}

afterEach(() => {
  // Every test sets its own starting URL; leaving one behind would let it
  // leak into whichever test runs next.
  window.history.pushState({}, "", "/");
});

describe("AdoSignInBanner", () => {
  it("renders nothing with neither query param present", () => {
    setURL("/runs");
    render(<AdoSignInBanner />);
    expect(screen.queryByText(ADO_SIGNIN.CONNECTED)).toBeNull();
  });

  it("ado_signin=connected: the success sentence, and the param is cleared from the address bar", () => {
    setURL("/?ado_signin=connected");
    render(<AdoSignInBanner />);
    expect(screen.getByText(ADO_SIGNIN.CONNECTED)).toBeInTheDocument();
    expect(window.location.search).toBe("");
  });

  it("dismissing the success banner removes it", async () => {
    setURL("/?ado_signin=connected");
    render(<AdoSignInBanner />);
    await userEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText(ADO_SIGNIN.CONNECTED)).toBeNull();
  });

  // The five reasons that keep the plain retry — one representative case
  // (consent_required) plus a table walk of the rest for the exact sentence.
  it.each(Object.entries(ADO_SIGNIN.REASON).filter(([reason]) => reason !== "identity_binding"))(
    "ado_signin_error=%s: its own sentence, and Try again carries no prompt param",
    (reason, sentence) => {
      setURL(`/?ado_signin_error=${reason}`);
      render(<AdoSignInBanner />);
      expect(screen.getByText(sentence)).toBeInTheDocument();
      const retry = screen.getByRole("link", { name: ADO_SIGNIN.RETRY });
      expect(retry).toHaveAttribute("href", "/api/v1/scm/azure-devops/signin");
      expect(window.location.search).toBe("");
    },
  );

  // Q2: identity_binding alone adds prompt=select_account — the one retry
  // that would otherwise likely hit the same wrong Microsoft account silently.
  it("ado_signin_error=identity_binding: its own sentence, and Try again asks for the account picker", () => {
    setURL("/?ado_signin_error=identity_binding");
    render(<AdoSignInBanner />);
    expect(screen.getByText(ADO_SIGNIN.REASON.identity_binding)).toBeInTheDocument();
    const retry = screen.getByRole("link", { name: ADO_SIGNIN.RETRY });
    expect(retry).toHaveAttribute("href", "/api/v1/scm/azure-devops/signin?prompt=select_account");
  });

  // An older daemon's code, or any value this console does not know, reads
  // as exchange_failed's own sentence rather than nothing at all.
  it("an unrecognised reason falls back to the exchange_failed sentence", () => {
    setURL("/?ado_signin_error=some_future_reason");
    render(<AdoSignInBanner />);
    expect(screen.getByText(ADO_SIGNIN.REASON.exchange_failed)).toBeInTheDocument();
  });

  it("keeps the rest of the query string, only stripping its own two params", () => {
    setURL("/runs/new?prefill=1&ado_signin_error=exchange_failed");
    render(<AdoSignInBanner />);
    expect(screen.getByText(ADO_SIGNIN.REASON.exchange_failed)).toBeInTheDocument();
    expect(window.location.search).toBe("?prefill=1");
    expect(window.location.pathname).toBe("/runs/new");
  });
});
