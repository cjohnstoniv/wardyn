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

import {
  EMAIL_DOMAIN_REFUSAL,
  SignIn,
  TOKEN_HINT,
  UNREACHABLE_ERROR,
} from "./sign-in";
import { SESSION_ENDED_REASON } from "../../lib/api/core";

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

  // F3-F10: role is three-valued since 0.7 SSO Phase 3 — the sentence used to
  // name only "admin or member", telling a security admin they'd get a role
  // that isn't theirs.
  it("F3-F10: the SSO caveat names all three roles, not just admin/member", async () => {
    healthMock.mockResolvedValue({ sso: true });
    renderSignIn();
    await screen.findByRole("link", { name: /sign in with sso/i });
    expect(screen.getByText(/admin, security admin or member/i)).toBeInTheDocument();
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

// #378/#379: the sign-in screen reads /healthz's new token_login/sso_only bits
// and renders only what can work, in every combination the posture can report.
describe("SignIn — renders only what the posture says can work (#378/#379)", () => {
  it("SSO-only: one 'Sign in with SSO' button, no admin-token field, no role-source caveat", async () => {
    healthMock.mockResolvedValue({ sso: true, sso_only: true, token_login: false });
    renderSignIn();
    await screen.findByRole("link", { name: /sign in with sso/i });
    expect(screen.queryByLabelText(/admin token/i)).not.toBeInTheDocument();
    expect(
      screen.queryByText(/comes from your SSO role assignment/i),
    ).not.toBeInTheDocument();
  });

  it("token + SSO both configured (today's combined deployment): unchanged", async () => {
    healthMock.mockResolvedValue({ sso: true, sso_only: false, token_login: true });
    renderSignIn();
    await screen.findByRole("link", { name: /sign in with sso/i });
    expect(screen.getByLabelText(/admin token/i)).toBeInTheDocument();
    expect(
      screen.getByText(/comes from your SSO role assignment/i),
    ).toBeInTheDocument();
  });

  it("both bits false keeps the admin-token form (nothing says sign-in is unavailable)", async () => {
    healthMock.mockResolvedValue({ sso: false, sso_only: false, token_login: false });
    renderSignIn();
    expect(await screen.findByLabelText(/admin token/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /sign in with sso/i })).toBeDisabled();
  });

  it("local-mode cell: an older/local daemon reporting no posture bits keeps today's form", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
    renderSignIn();
    expect(await screen.findByLabelText(/admin token/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /sign in with sso/i })).toBeDisabled();
  });

  // The one cell that actually CHANGES behavior for an install that already
  // exists: an ordinary OIDC deployment with no admin token configured (or
  // member mode — either way token_login is false while sso stays true).
  // Before #378/#379 this rendered the admin-token field regardless, so
  // every submission there was refused with "admin token not configured".
  it("OIDC configured with no usable token (token_login false, sso_only false): the admin-token field disappears", async () => {
    healthMock.mockResolvedValue({ sso: true, sso_only: false, token_login: false });
    renderSignIn();
    await screen.findByRole("link", { name: /sign in with sso/i });
    expect(screen.queryByLabelText(/admin token/i)).not.toBeInTheDocument();
    // sso_only is false here, so the role-source caveat still belongs on screen.
    expect(
      screen.getByText(/comes from your SSO role assignment/i),
    ).toBeInTheDocument();
  });

  // R4/F027's failure shape, replayed for the two new bits: health() resolves
  // the EMPTY object on a network error or any non-2xx, and the gate must
  // leave its LAST KNOWN state alone rather than read "no answer" as "nothing
  // works" — that early-return path is exactly what once left an SSO-only
  // deployment showing no way in at all when the daemon merely hadn't
  // answered yet (see refreshSso's comment in sign-in.tsx).
  it("a failed /healthz on mount does not hide the admin-token form or misreport the posture", async () => {
    healthMock.mockResolvedValue({});
    renderSignIn();
    expect(await screen.findByLabelText(/admin token/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /sign in with sso/i })).toBeDisabled();
  });
});

// #212 (design/first-contact-prototype): the sign-in screen used to
// advertise a working demo credential (`demo-admin-token`, in both the
// placeholder and the hint) and named the env var it reads
// (WARDYN_ADMIN_TOKEN) — internals a reader who has not authenticated has no
// business seeing. The hint now says what belongs in the field and where the
// person saw it, with neither the env var name nor the compose token.
describe("SignIn — the token field advertises no working credential or env var (#212)", () => {
  it("the hint says what belongs in the field and where it came from, naming no env var and no demo token", async () => {
    healthMock.mockResolvedValue({});
    renderSignIn();
    expect(await screen.findByText(TOKEN_HINT)).toBeInTheDocument();
    expect(screen.queryByText(/WARDYN_ADMIN_TOKEN/)).not.toBeInTheDocument();
    expect(screen.queryByText(/demo-admin-token/)).not.toBeInTheDocument();
  });

  it("the token input's placeholder carries no working demo credential", () => {
    healthMock.mockResolvedValue({});
    renderSignIn();
    const input = screen.getByLabelText(/admin token/i);
    expect(input).not.toHaveAttribute("placeholder", "demo-admin-token");
    expect(input.getAttribute("placeholder") ?? "").toBe("");
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

  it("a network error (daemon down / unreachable) shows a reachability message naming Wardyn and one thing to check, not the rejected-token copy (#212)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to fetch")));
    await submit();
    expect(await screen.findByText(UNREACHABLE_ERROR)).toBeInTheDocument();
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

  it("renders the email_domain message, pointing this locked-out reader at their admin rather than an env var they cannot reach (#212)", async () => {
    window.history.pushState({}, "", "/?auth_error=email_domain");
    healthMock.mockResolvedValue({});
    renderSignIn();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(EMAIL_DOMAIN_REFUSAL);
    expect(alert).not.toHaveTextContent(/WARDYN_OIDC_EMAIL_DOMAINS/);
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

// X3-F7: App.tsx's onUnauthorized handler now hands SignIn a `reason` for a
// mid-session expiry — rendered in the SAME alert slot submitToken's own
// failures use, so the gate stops reading as a silent, unexplained teleport.
describe("SignIn — a mid-session expiry's reason (X3-F7)", () => {
  it("renders the reason prop in the alert slot on mount", async () => {
    window.history.pushState({}, "", "/");
    healthMock.mockResolvedValue({});
    render(
      <ThemeProvider>
        <SignIn onSignIn={() => {}} reason={SESSION_ENDED_REASON} />
      </ThemeProvider>,
    );
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/your session ended/i);
  });

  // Negative control: the ordinary mount-probe gate (never signed in this tab
  // at all) passes no reason — must render exactly as it always did.
  it("neg: no reason prop means no alert on mount", () => {
    window.history.pushState({}, "", "/");
    healthMock.mockResolvedValue({});
    renderSignIn();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
