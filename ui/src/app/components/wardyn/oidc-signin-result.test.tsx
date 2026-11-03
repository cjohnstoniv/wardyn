/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Where an endpoint sign-in ends: the callback's redirect, read once and taken off the address. In the window the
// door opened it tells the door and closes; anywhere else it is a strip.
import { it, expect, vi, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { OidcSignInResult } from "./oidc-signin-result";
import { OIDC_RETURN, OIDC_SIGN_IN as T } from "./copy/oidc-signin";

afterEach(() => {
  vi.restoreAllMocks();
  window.history.replaceState(null, "", "/");
  Object.defineProperty(window, "opener", { value: null, configurable: true });
});

const at = (query: string) => window.history.replaceState(null, "", `/${query}`);

it("renders nothing, and touches nothing, on a page that is not a return", () => {
  at("?tab=x");
  const { container } = render(<OidcSignInResult />);
  expect(container).toBeEmptyDOMElement();
  expect(window.location.search).toBe("?tab=x");
});

it("with no window to tell, shows the success and clears the query", () => {
  at(`?${OIDC_RETURN.DONE_PARAM}=${OIDC_RETURN.DONE_VALUE}&provider=u-1&keep=1`);
  render(<OidcSignInResult />);
  expect(screen.getByText(T.BANNER_OK)).toBeInTheDocument();
  expect(window.location.search).toBe("?keep=1");
});

it.each([
  ["consent_required", T.FAIL.refused(T.BANNER_FAIL_NAME)],
  ["row_changed", T.FAIL.row_changed(T.BANNER_FAIL_NAME)],
  ["identity_binding", T.FAIL.identity_binding(T.BANNER_FAIL_NAME)],
  ["unusable_grant", T.FAIL.unusable_grant(T.BANNER_FAIL_NAME)],
  ["exchange_failed", T.FAIL.unreachable(T.BANNER_FAIL_NAME)],
  ["a_code_from_the_future", T.FAIL.unreachable(T.BANNER_FAIL_NAME)],
])("a %s return reads as its sentence", (reason, sentence) => {
  at(`?${OIDC_RETURN.ERROR_PARAM}=${reason}&provider=u-1`);
  render(<OidcSignInResult />);
  expect(screen.getByText(sentence)).toBeInTheDocument();
});

it("in the door's own window it tells the opener, by this origin, and closes", () => {
  const post = vi.fn();
  Object.defineProperty(window, "opener", { value: { postMessage: post }, configurable: true });
  const close = vi.spyOn(window, "close").mockImplementation(() => {});
  at(`?${OIDC_RETURN.ERROR_PARAM}=identity_binding&provider=u-1`);
  const { container } = render(<OidcSignInResult />);
  expect(post).toHaveBeenCalledWith({ type: OIDC_RETURN.MESSAGE, ok: false, reason: "identity_binding" }, window.location.origin);
  expect(close).toHaveBeenCalled();
  expect(container).toBeEmptyDOMElement();
});
