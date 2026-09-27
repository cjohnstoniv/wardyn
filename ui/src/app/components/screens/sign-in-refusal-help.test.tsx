/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #484 — the admin-written help under a sign-in refusal. Its own file so the
// refusal sentences' suite (sign-in.test.tsx) stays about the sentences.
import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { ThemeProvider } from "../wardyn/theme-provider";
import { SIGNIN_HELP_LINK_LABEL } from "../../lib/people-access-copy";

const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({
  health: { health: (...a: unknown[]) => healthMock(...a) },
}));

import { SignIn } from "./sign-in";

const TEXT = `Ask in #it-helpdesk — it's "Wardyn access" you want.`;
const URL = "https://it.corp.example/request";

async function renderWithRefusal(code: string, help: Record<string, string> = { sign_in_help_text: TEXT, sign_in_help_url: URL }) {
  healthMock.mockResolvedValue({ status: "ok", sso: true, ...help });
  window.history.pushState({}, "", `/?auth_error=${code}`);
  render(
    <ThemeProvider>
      <SignIn onSignIn={() => {}} />
    </ThemeProvider>,
  );
  // The alert renders on mount; the help lands with the /healthz answer.
  await screen.findByRole("link", { name: /sign in with sso/i });
}

afterEach(() => {
  healthMock.mockReset();
  window.history.pushState({}, "", "/");
});

describe("SignIn — admin-written help under a refusal (#484)", () => {
  it.each(["no_role", "email_domain", "claims_overage", "email_verified_absent"])(
    "shows the text and Request access under the %s refusal, after Wardyn's own sentence",
    async (code) => {
      await renderWithRefusal(code);
      const alert = screen.getByRole("alert");
      const help = screen.getByTestId("sign-in-help");
      expect(help).toHaveTextContent(TEXT);
      const link = within(help).getByRole("link", { name: SIGNIN_HELP_LINK_LABEL });
      expect(link).toHaveAttribute("href", URL);
      expect(link).toHaveAttribute("target", "_blank");
      expect(link).toHaveAttribute("rel", "noopener noreferrer");
      // Wardyn's sentence stays first, and is never replaced by the admin's.
      expect(alert.compareDocumentPosition(help) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
      expect(alert).not.toHaveTextContent(TEXT);
    },
  );

  it.each(["oidc_transient", "oidc_config", "email_unverified", "role_check_unavailable", "something_new"])(
    "shows nothing extra under the %s refusal",
    async (code) => {
      await renderWithRefusal(code);
      expect(screen.getByRole("alert")).toBeInTheDocument();
      expect(screen.queryByTestId("sign-in-help")).not.toBeInTheDocument();
      expect(screen.queryByText(TEXT)).not.toBeInTheDocument();
    },
  );

  it("renders markup literally — text, never HTML", async () => {
    const markup = `<a href="https://evil.example">click</a><img src=x onerror=alert(1)>`;
    await renderWithRefusal("no_role", { sign_in_help_text: markup });
    const help = screen.getByTestId("sign-in-help");
    expect(help).toHaveTextContent(markup);
    expect(help.querySelector("img")).toBeNull();
    expect(within(help).queryByRole("link")).not.toBeInTheDocument();
  });

  it("never links a non-http(s) address", async () => {
    await renderWithRefusal("no_role", { sign_in_help_url: "javascript:alert(1)" });
    expect(screen.queryByRole("link", { name: SIGNIN_HELP_LINK_LABEL })).not.toBeInTheDocument();
  });

  it("text alone, no link, when only the text is set", async () => {
    await renderWithRefusal("no_role", { sign_in_help_text: TEXT });
    expect(screen.getByTestId("sign-in-help")).toHaveTextContent(TEXT);
    expect(screen.queryByRole("link", { name: SIGNIN_HELP_LINK_LABEL })).not.toBeInTheDocument();
  });

  it("nothing extra when the admin set nothing", async () => {
    await renderWithRefusal("no_role", {});
    expect(screen.queryByTestId("sign-in-help")).not.toBeInTheDocument();
  });
});
