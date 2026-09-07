/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, afterEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ThemeProvider } from "../wardyn/theme-provider";
import { getToken } from "../../lib/api/core";

// The server-side OIDC flow (GET /auth/login) ships whenever WARDYN_OIDC_* is
// set, and its session cookie authenticates the whole API — but the console used
// to hard-disable the SSO button, so an OIDC-only deployment (no admin token) had
// NO way in. The button must follow /healthz's `sso`, never a hard-coded stance.
const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({
  health: { health: (...a: unknown[]) => healthMock(...a) },
}));

import { SignIn } from "./sign-in";

function renderSignIn() {
  return render(
    <ThemeProvider>
      <SignIn onSignIn={() => {}} />
    </ThemeProvider>,
  );
}

describe("SignIn — SSO entry point", () => {
  it("links to /auth/login when the control plane reports OIDC configured", async () => {
    healthMock.mockResolvedValue({ sso: true });
    renderSignIn();
    const link = await screen.findByRole("link", { name: /sign in with sso/i });
    expect(link).toHaveAttribute("href", "/auth/login");
    // The caveat under the SSO button: a role (admin/member) is derived from the
    // caller's SSO role assignment (admin/member RBAC shipped in v0.5).
    expect(screen.getByText(/comes from your SSO role assignment/i)).toBeInTheDocument();
  });

  it("stays disabled when OIDC is not configured", async () => {
    healthMock.mockResolvedValue({});
    renderSignIn();
    expect(await screen.findByRole("button", { name: /sign in with sso/i })).toBeDisabled();
    expect(screen.queryByRole("link", { name: /sign in with sso/i })).not.toBeInTheDocument();
  });

  // R4/F027: health() resolves the EMPTY object for a network error or ANY
  // non-2xx, and the gate read it ONCE on mount. So a daemon that was still
  // starting — the single likeliest moment for a human to be sitting on this
  // screen — made an SSO-ONLY deployment render a bare admin-token field and no
  // way in, permanently, because nothing ever asked again. "Couldn't ask" is not
  // "not configured": the gate keeps the last known answer and re-asks.
  it("an outage does not turn the SSO button off — the gate re-asks and it comes back", async () => {
    vi.useFakeTimers();
    try {
      // First answer is the outage shape, then the daemon comes up.
      healthMock.mockReset();
      healthMock.mockResolvedValueOnce({}).mockResolvedValue({ status: "ok", sso: true });
      renderSignIn();
      // The outage answer has landed and taught the gate nothing — no link, and
      // (this is the point) no permanent "not configured" verdict either.
      await act(async () => void (await vi.advanceTimersByTimeAsync(0)));
      expect(healthMock).toHaveBeenCalledTimes(1);
      expect(screen.queryByRole("link", { name: /sign in with sso/i })).not.toBeInTheDocument();

      await act(async () => void (await vi.advanceTimersByTimeAsync(10_000)));
      await vi.waitFor(() =>
        expect(screen.getByRole("link", { name: /sign in with sso/i })).toHaveAttribute(
          "href",
          "/auth/login",
        ),
      );
    } finally {
      vi.useRealTimers();
    }
  });

  // The other half of the same rule: a LATER outage must not retract an answer
  // the daemon already gave. Losing the SSO link mid-outage is the same dead end
  // as never rendering it.
  it("a later outage does not retract an SSO answer the daemon already gave", async () => {
    vi.useFakeTimers();
    try {
      healthMock.mockReset();
      healthMock.mockResolvedValueOnce({ status: "ok", sso: true }).mockResolvedValue({});
      renderSignIn();
      await vi.waitFor(() =>
        expect(screen.getByRole("link", { name: /sign in with sso/i })).toBeInTheDocument(),
      );
      await act(async () => void (await vi.advanceTimersByTimeAsync(30_000)));
      expect(healthMock.mock.calls.length).toBeGreaterThan(1);
      expect(screen.getByRole("link", { name: /sign in with sso/i })).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });
});

// W31-S1-2 regression: wardynd never prints an admin token on startup — it
// only ever READS WARDYN_ADMIN_TOKEN from the environment (cmd/wardynd's
// boot_flags.go/main.go). The sign-in copy claiming otherwise was the gate's
// only instruction AND part of the threat model's own token-provenance claim
// (THREAT-MODEL.md's "Console auth token storage" section, fixed alongside
// this file); a false instruction here is a bad-first-run trap that sends an
// operator hunting server logs for output that will never appear.
describe("SignIn — admin token instructions are honest about provenance", () => {
  it("tells the operator to paste the token the control plane was STARTED WITH, not one wardynd printed", async () => {
    healthMock.mockResolvedValue({});
    renderSignIn();
    expect(
      await screen.findByText(/paste the token this control plane was started with/i),
    ).toBeInTheDocument();
    expect(screen.getByText(/WARDYN_ADMIN_TOKEN/)).toBeInTheDocument();
    expect(screen.queryByText(/wardynd printed/i)).not.toBeInTheDocument();
  });

  it("the token input's placeholder carries no fake fixed-prefix format", () => {
    healthMock.mockResolvedValue({});
    renderSignIn();
    const input = screen.getByLabelText(/admin token/i);
    // Real admin tokens are whatever the operator set WARDYN_ADMIN_TOKEN to
    // (e.g. openssl rand -hex 32) — there is no "wardyn_admin_" value prefix;
    // that string is only the UNRELATED localStorage key name (core.ts).
    expect(input).toHaveAttribute("placeholder", "demo-admin-token");
  });
});

// W31-S1-4: every submitToken failure used to collapse to probeAuth's plain
// boolean, so a daemon 5xx and an unreachable control plane both rendered the
// SAME "That admin token was rejected" copy as an actually-bad token — and
// cleared a token that may have been perfectly valid.
describe("SignIn — submitToken tells a rejected token apart from a reachability failure", () => {
  afterEach(() => vi.unstubAllGlobals());

  async function submit(token = "sometoken") {
    sessionStorage.clear();
    localStorage.clear();
    healthMock.mockResolvedValue({});
    renderSignIn();
    await userEvent.type(screen.getByLabelText(/admin token/i), token);
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
  }

  it("a real 401 shows the rejected-token copy and clears the stored token", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response("", { status: 401, statusText: "Unauthorized" })),
    );
    await submit();
    expect(await screen.findByText(/that admin token was rejected/i)).toBeInTheDocument();
    expect(getToken()).toBeNull();
  });

  it("a 500 from a live daemon shows the server's OWN message, not the rejected-token copy", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ error: "database unavailable" }), { status: 500 }),
      ),
    );
    await submit();
    expect(await screen.findByText("database unavailable")).toBeInTheDocument();
    expect(screen.queryByText(/that admin token was rejected/i)).not.toBeInTheDocument();
    // A 5xx is not proof the token is bad — keep it.
    expect(getToken()).toBe("sometoken");
  });

  it("a network error (daemon down / unreachable) shows a reachability message, not the rejected-token copy", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to fetch")));
    await submit();
    expect(await screen.findByText(/could not reach the control plane/i)).toBeInTheDocument();
    expect(screen.queryByText(/that admin token was rejected/i)).not.toBeInTheDocument();
    expect(getToken()).toBe("sometoken");
  });
});

// W31-S1-5 (re-fix): the OIDC callback redirects a user-actionable login
// denial to "/?auth_error=<code>" instead of a bare http.Error text page —
// but that redirect lands right back on THIS screen, so if nothing here reads
// the code the user sees a plain sign-in form with zero explanation, no
// better than the dead end it replaced. SignIn must render the mapped
// message inline.
describe("SignIn — renders the OIDC callback's ?auth_error=<code> inline (W31-S1-5)", () => {
  afterEach(() => {
    window.history.replaceState({}, "", "/");
  });

  it("renders the no_role message and strips the param from the URL", async () => {
    window.history.pushState({}, "", "/?auth_error=no_role");
    healthMock.mockResolvedValue({});
    renderSignIn();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/no wardyn role assigned/i);
    // §7.7's rule: name "your Wardyn admin" as who to ask — a signed-in human
    // should never be told to go set an env var themselves.
    expect(alert).toHaveTextContent(/ask your wardyn admin/i);
    expect(window.location.search).toBe("");
  });

  it("renders the email_domain message", async () => {
    window.history.pushState({}, "", "/?auth_error=email_domain");
    healthMock.mockResolvedValue({});
    renderSignIn();
    expect(await screen.findByRole("alert")).toHaveTextContent(/domain isn't allowed/i);
  });

  it("renders the email_unverified message", async () => {
    window.history.pushState({}, "", "/?auth_error=email_unverified");
    healthMock.mockResolvedValue({});
    renderSignIn();
    expect(await screen.findByRole("alert")).toHaveTextContent(/unverified/i);
  });

  // C1: absent must render its OWN message — never conflated with
  // email_unverified (the IdP explicitly said unverified) — and must name
  // the real remedy (WARDYN_OIDC_ROLE_MAP), not domains.
  it("renders the email_verified_absent message, distinct from email_unverified", async () => {
    window.history.pushState({}, "", "/?auth_error=email_verified_absent");
    healthMock.mockResolvedValue({});
    renderSignIn();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/doesn't send an email_verified claim/i);
    expect(alert).toHaveTextContent(/WARDYN_OIDC_ROLE_MAP/);
    expect(alert).toHaveTextContent(/ask your wardyn admin/i);
  });

  // New arm (0.7 SSO Phase 3, §7.7): the People preview panel's own
  // "couldn't check" language, shared here for the same failure shape.
  it("renders the role_check_unavailable message", async () => {
    window.history.pushState({}, "", "/?auth_error=role_check_unavailable");
    healthMock.mockResolvedValue({});
    renderSignIn();
    expect(await screen.findByRole("alert")).toHaveTextContent(/couldn't check your access/i);
  });

  // claims_overage must NOT reach the generic fallback: that arm says "Try
  // again", and retrying replays the identical token. The remedy is the
  // admin's, so the copy has to name it.
  it("renders the claims_overage message and does not tell the user to retry", async () => {
    window.history.pushState({}, "", "/?auth_error=claims_overage");
    healthMock.mockResolvedValue({});
    renderSignIn();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/too many groups/i);
    expect(alert).toHaveTextContent(/ask your wardyn admin/i);
    expect(alert).not.toHaveTextContent(/sign-in failed/i);
  });

  it("falls back to a generic message for an unrecognized code", async () => {
    window.history.pushState({}, "", "/?auth_error=something_new");
    healthMock.mockResolvedValue({});
    renderSignIn();
    expect(await screen.findByRole("alert")).toHaveTextContent(/sign-in failed/i);
  });

  it("shows no error banner when the URL carries no auth_error", () => {
    window.history.pushState({}, "", "/");
    healthMock.mockResolvedValue({});
    renderSignIn();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
