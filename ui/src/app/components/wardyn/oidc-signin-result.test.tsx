/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Where an endpoint sign-in ends: the callback's redirect, read once and taken off the address. In the window the
// door opened it tells the door and closes; anywhere else it is a strip.
import { it, expect, vi, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { OidcSignInResult } from "./oidc-signin-result";
import { ModelAccessProvider } from "./model-access-context";
import { providerStatus } from "../../lib/test-fixtures";
import { OIDC_RETURN, OIDC_SIGN_IN as T } from "./copy/oidc-signin";

afterEach(() => {
  vi.restoreAllMocks();
  window.history.replaceState(null, "", "/");
  Object.defineProperty(window, "opener", { value: null, configurable: true });
  window.name = "";
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
  ["identity_issuer_mismatch", T.FAIL.issuer_mismatch(T.BANNER_FAIL_NAME)],
  ["store_error", "Wardyn couldn't save your sign-in. Try again."],
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
  window.name = "wardyn-provider-oidc-abc123";
  at(`?${OIDC_RETURN.ERROR_PARAM}=identity_binding&provider=u-1`);
  render(<OidcSignInResult />);
  expect(post).toHaveBeenCalledWith(
    { type: OIDC_RETURN.MESSAGE, ok: false, reason: "identity_binding", attempt: "wardyn-provider-oidc-abc123" },
    window.location.origin,
  );
  expect(close).toHaveBeenCalled();
  // A window the browser leaves open says it can be closed.
  expect(screen.getByText(T.WINDOW_FAIL(T.BANNER_FAIL_NAME))).toBeInTheDocument();
});

it("in the door's own window a success says it can be closed", () => {
  Object.defineProperty(window, "opener", { value: { postMessage: vi.fn() }, configurable: true });
  vi.spyOn(window, "close").mockImplementation(() => {});
  at(`?${OIDC_RETURN.DONE_PARAM}=${OIDC_RETURN.DONE_VALUE}&provider=u-1`);
  render(<OidcSignInResult />);
  expect(screen.getByText(T.WINDOW_OK(T.BANNER_FAIL_NAME))).toBeInTheDocument();
});

it("the return banner is announced: a status for success, an alert for a failure", () => {
  at(`?${OIDC_RETURN.DONE_PARAM}=${OIDC_RETURN.DONE_VALUE}&provider=u-1`);
  const ok = render(<OidcSignInResult />);
  expect(screen.getByRole("status")).toHaveTextContent(T.BANNER_OK);
  expect(screen.queryByRole("alert")).toBeNull();
  ok.unmount();
  at(`?${OIDC_RETURN.ERROR_PARAM}=consent_required&provider=u-1`);
  render(<OidcSignInResult />);
  expect(screen.getByRole("alert")).toHaveTextContent(T.FAIL.refused(T.BANNER_FAIL_NAME));
  expect(screen.queryByRole("status")).toBeNull();
});

const SSO = {
  id: "corp-sso", name: "Corp SSO", kind: "custom_endpoint", harnesses: ["claude-code"], host: "llm.contoso.example",
  sign_in: "oidc", uid: "u-1",
};
const withStatus = () => (
  <ModelAccessProvider status={providerStatus([{ provider: SSO }])} onRefresh={() => {}}>
    <OidcSignInResult />
  </ModelAccessProvider>
);

it("with no opener, the strip names the provider the return's uid is, when setup status lists it", () => {
  at(`?${OIDC_RETURN.DONE_PARAM}=${OIDC_RETURN.DONE_VALUE}&provider=u-1`);
  const ok = render(withStatus());
  expect(screen.getByText(T.BANNER_OK_NAMED("Corp SSO"))).toBeInTheDocument();
  ok.unmount();
  at(`?${OIDC_RETURN.ERROR_PARAM}=identity_binding&provider=u-1`);
  render(withStatus());
  expect(screen.getByText(T.FAIL.identity_binding("Corp SSO"))).toBeInTheDocument();
});

it("a uid setup status does not list reads as the unnamed strip", () => {
  at(`?${OIDC_RETURN.ERROR_PARAM}=row_changed&provider=u-gone`);
  render(withStatus());
  expect(screen.getByText(T.FAIL.row_changed(T.BANNER_FAIL_NAME))).toBeInTheDocument();
});
