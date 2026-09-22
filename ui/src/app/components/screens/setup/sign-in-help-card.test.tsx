/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { HttpError } from "../../../lib/api/core";
import { SIGNIN_HELP } from "../../../lib/access-posture-copy";
import { SIGNIN } from "../../../lib/people-access-copy";
import { OperatorProvider } from "../../wardyn/operator-context";

const getSnapshot = vi.fn();
const putSiteConfig = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: {
    getSiteConfigSnapshot: (...a: unknown[]) => getSnapshot(...a),
    putSiteConfig: (...a: unknown[]) => putSiteConfig(...a),
  },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn(), warning: vi.fn() } }));

import { helpTextLength, SignInHelpCard } from "./sign-in-help-card";

const BASE = { scm_hosts: ["github.example.com"], upstream_proxy_url: "http://proxy.corp.example:3128" };

function renderCard(operator = true) {
  return render(
    <OperatorProvider operator={operator}>
      <SignInHelpCard />
    </OperatorProvider>,
  );
}

beforeEach(() => {
  getSnapshot.mockReset().mockResolvedValue({ siteConfig: { ...BASE }, etag: '"v1"' });
  putSiteConfig.mockReset().mockImplementation(async (cfg: Record<string, unknown>) => ({
    siteConfig: cfg,
    etag: '"v2"',
    danglingSecretRefs: [],
    onboardingCompletedAtIgnored: false,
    appliesFrom: "",
    sourcesNoLongerAdmitted: null,
  }));
});

describe("SignInHelpCard (#484)", () => {
  it("renders the frozen strings, the empty note and the no-role preview", async () => {
    renderCard();
    expect(await screen.findByText(SIGNIN_HELP.TITLE)).toBeInTheDocument();
    expect(screen.getByText(SIGNIN_HELP.LEAD)).toBeInTheDocument();
    expect(screen.getByLabelText(SIGNIN_HELP.TEXT_LABEL)).toHaveAttribute("placeholder", SIGNIN_HELP.TEXT_PLACEHOLDER);
    expect(screen.getByText(SIGNIN_HELP.TEXT_HINT)).toBeInTheDocument();
    expect(screen.getByLabelText(SIGNIN_HELP.URL_LABEL)).toHaveAttribute("placeholder", SIGNIN_HELP.URL_PLACEHOLDER);
    expect(screen.getByText(SIGNIN_HELP.URL_HINT)).toBeInTheDocument();
    expect(screen.getByText(SIGNIN_HELP.EMPTY_NOTE)).toBeInTheDocument();
    expect(screen.getByText(SIGNIN_HELP.PREVIEW_HEADING)).toBeInTheDocument();
    expect(screen.getByText(SIGNIN.NO_ROLE)).toBeInTheDocument();
    expect(screen.getByText(SIGNIN_HELP.APPLIES_NOTE)).toBeInTheDocument();
    // No counter until the admin types.
    expect(screen.queryByText("0 / 1000")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: SIGNIN_HELP.SAVE })).toBeDisabled();
  });

  it("renders nothing for a non-admin, and never reads the document", () => {
    const { container } = renderCard(false);
    expect(container).toBeEmptyDOMElement();
    expect(getSnapshot).not.toHaveBeenCalled();
  });

  it("counts characters once typing, previews the text and Request access, and folds line breaks", async () => {
    renderCard();
    const text = await screen.findByLabelText(SIGNIN_HELP.TEXT_LABEL);
    await userEvent.type(text, "Ask IT{enter}now");
    expect(text).toHaveValue("Ask IT now");
    expect(screen.getByText("10 / 1000")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText(SIGNIN_HELP.URL_LABEL), "https://it.corp.example/");
    const preview = screen.getByTestId("sign-in-help");
    expect(preview).toHaveTextContent("Ask IT now");
    expect(within(preview).getByRole("link", { name: SIGNIN_HELP.LINK_LABEL })).toHaveAttribute(
      "href",
      "https://it.corp.example/",
    );
    expect(screen.queryByText(SIGNIN_HELP.EMPTY_NOTE)).not.toBeInTheDocument();
  });

  it("counts characters, not UTF-16 units", () => {
    expect(helpTextLength("é😀")).toBe(2);
  });

  it("withholds Save over 1,000 characters and for a non-http(s) link", async () => {
    renderCard();
    const text = await screen.findByLabelText(SIGNIN_HELP.TEXT_LABEL);
    const save = screen.getByRole("button", { name: SIGNIN_HELP.SAVE });
    await userEvent.click(text);
    await userEvent.paste("a".repeat(1000));
    expect(screen.getByText("1000 / 1000")).toBeInTheDocument();
    expect(save).toBeEnabled();
    await userEvent.type(text, "b");
    expect(screen.getByText("1001 / 1000")).toBeInTheDocument();
    expect(save).toBeDisabled();
    await userEvent.type(text, "{backspace}");
    await userEvent.type(screen.getByLabelText(SIGNIN_HELP.URL_LABEL), "javascript:alert(1)");
    expect(save).toBeDisabled();
    expect(screen.queryByRole("link", { name: SIGNIN_HELP.LINK_LABEL })).not.toBeInTheDocument();
  });

  it("saves the whole document with If-Match, changing only the two fields", async () => {
    renderCard();
    await userEvent.type(await screen.findByLabelText(SIGNIN_HELP.TEXT_LABEL), `It's "quick" — ask IT.`);
    await userEvent.type(screen.getByLabelText(SIGNIN_HELP.URL_LABEL), "https://it.corp.example/");
    await userEvent.click(screen.getByRole("button", { name: SIGNIN_HELP.SAVE }));
    expect(putSiteConfig).toHaveBeenCalledWith(
      { ...BASE, sign_in_help_text: `It's "quick" — ask IT.`, sign_in_help_url: "https://it.corp.example/" },
      '"v1"',
    );
    // Saved: nothing left to save.
    expect(await screen.findByRole("button", { name: SIGNIN_HELP.SAVE })).toBeDisabled();
  });

  it("on a 412 keeps the draft, reloads the document underneath and says so", async () => {
    putSiteConfig.mockRejectedValueOnce(new HttpError(412, "If-Match does not match"));
    renderCard();
    const text = await screen.findByLabelText(SIGNIN_HELP.TEXT_LABEL);
    await userEvent.type(text, "Mine");
    getSnapshot.mockResolvedValueOnce({ siteConfig: { ...BASE, scm_hosts: ["other.example.com"] }, etag: '"v9"' });
    await userEvent.click(screen.getByRole("button", { name: SIGNIN_HELP.SAVE }));
    expect(await screen.findByText(SIGNIN_HELP.SAVED_ELSEWHERE)).toBeInTheDocument();
    expect(text).toHaveValue("Mine");
    await userEvent.click(screen.getByRole("button", { name: SIGNIN_HELP.SAVE }));
    expect(putSiteConfig).toHaveBeenLastCalledWith(
      { ...BASE, scm_hosts: ["other.example.com"], sign_in_help_text: "Mine", sign_in_help_url: "" },
      '"v9"',
    );
  });

  it("shows the server's 400 verbatim", async () => {
    const msg = "invalid site config: sign_in_help_url: must be an http:// or https:// address — it is shown to people who have not signed in";
    putSiteConfig.mockRejectedValueOnce(new HttpError(400, msg));
    renderCard();
    await userEvent.type(await screen.findByLabelText(SIGNIN_HELP.TEXT_LABEL), "x");
    await userEvent.click(screen.getByRole("button", { name: SIGNIN_HELP.SAVE }));
    expect(await screen.findByRole("alert")).toHaveTextContent(msg);
  });
});
