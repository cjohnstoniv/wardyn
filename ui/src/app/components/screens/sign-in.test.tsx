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
import { SIGNIN } from "../../lib/sign-in-copy";
import { STATES } from "../wardyn/states";
import { SESSION_ENDED_REASON } from "../../lib/api/core";

function renderSignIn() {
  return render(
    <ThemeProvider>
      <SignIn onSignIn={() => {}} />
    </ThemeProvider>,
  );
}

// #457 (docs/design/signin-first-contact-canon.md, states 1-2): before
// /healthz has answered even once, the screen makes no claim about which
// doors exist — no token field, no SSO control, just an honest "checking"
// row. After three unanswered reads it says the checking is still going.
describe("SignIn — checking state before any answer (#457)", () => {
  it("Q457-1: shows only the checking row — no token field, no SSO control, no claim either way", async () => {
    healthMock.mockResolvedValue({}); // never answers
    renderSignIn();
    expect(await screen.findByText(SIGNIN.CHECKING)).toBeInTheDocument();
    expect(screen.queryByText(SIGNIN.STILL_CHECKING)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/admin token/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /sign in with sso/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /sign in with sso/i })).not.toBeInTheDocument();
  });

  it("Q457-2: adds the still-checking line only after three unanswered reads, not before", async () => {
    vi.useFakeTimers();
    try {
      healthMock.mockReset();
      healthMock.mockResolvedValue({}); // never answers
      renderSignIn();

      // Read #1 (mount).
      await act(async () => void (await vi.advanceTimersByTimeAsync(0)));
      expect(screen.getByText(SIGNIN.CHECKING)).toBeInTheDocument();
      expect(screen.queryByText(SIGNIN.STILL_CHECKING)).not.toBeInTheDocument();

      // Read #2.
      await act(async () => void (await vi.advanceTimersByTimeAsync(10_000)));
      expect(screen.queryByText(SIGNIN.STILL_CHECKING)).not.toBeInTheDocument();

      // Read #3 — the still-checking line appears, and not before.
      await act(async () => void (await vi.advanceTimersByTimeAsync(10_000)));
      expect(screen.getByText(SIGNIN.STILL_CHECKING)).toBeInTheDocument();
      expect(screen.queryByText(SIGNIN.CHECKING)).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it("a real answer ends the checking state and renders the doors it names", async () => {
    healthMock.mockResolvedValue({ status: "ok", sso: true });
    renderSignIn();
    await screen.findByRole("link", { name: /sign in with sso/i });
    expect(screen.queryByText(SIGNIN.CHECKING)).not.toBeInTheDocument();
    expect(screen.queryByText(SIGNIN.STILL_CHECKING)).not.toBeInTheDocument();
  });
});

describe("SignIn — SSO entry point", () => {
  it("links to /auth/login when the control plane reports OIDC configured", async () => {
    healthMock.mockResolvedValue({ sso: true });
    renderSignIn();
    const link = await screen.findByRole("link", { name: /sign in with sso/i });
    expect(link).toHaveAttribute("href", "/auth/login");
  });

  // #457: SIGNIN.ROLE_SOURCE (the "comes from your SSO role assignment"
  // caveat) is REMOVED everywhere, not just conditionally hidden — no cell
  // renders it any more, combined deployment included.
  it("#457: renders no role-source caveat under the SSO button, even with SSO and the token form both configured", async () => {
    healthMock.mockResolvedValue({ sso: true, sso_only: false, token_login: true });
    renderSignIn();
    await screen.findByRole("link", { name: /sign in with sso/i });
    expect(screen.queryByText(/SSO role assignment/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/admin, security admin or member/i)).not.toBeInTheDocument();
  });

  // #457: a disabled "Sign in with SSO" stub (and its WARDYN_OIDC_* title)
  // used to render whenever OIDC wasn't configured — now nothing renders at
  // all: no disabled button, no "isn't configured" sentence naming a chart
  // value this reader cannot reach.
  it("#457: renders no SSO control at all when OIDC is not configured — no disabled stub", async () => {
    healthMock.mockResolvedValue({ status: "ok", sso: false });
    renderSignIn();
    await screen.findByLabelText(/admin token/i);
    expect(screen.queryByRole("button", { name: /sign in with sso/i })).not.toBeInTheDocument();
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

  // #457: the role-source caveat is gone from this cell too now — SIGNIN.ROLE_SOURCE
  // is removed everywhere, not conditional on sso_only any more.
  it("token + SSO both configured (today's combined deployment): both doors render, no role-source caveat", async () => {
    healthMock.mockResolvedValue({ sso: true, sso_only: false, token_login: true });
    renderSignIn();
    await screen.findByRole("link", { name: /sign in with sso/i });
    expect(screen.getByLabelText(/admin token/i)).toBeInTheDocument();
    expect(
      screen.queryByText(/comes from your SSO role assignment/i),
    ).not.toBeInTheDocument();
  });

  // #457 state 5: both doors — token form, an "OR" divider, and the SSO
  // link, with the token form's submit as the ONE primary (teal) button.
  it("#457: both doors show the OR divider, and only the token submit is the primary button", async () => {
    healthMock.mockResolvedValue({ sso: true, sso_only: false, token_login: true });
    renderSignIn();
    const ssoLink = await screen.findByRole("link", { name: /sign in with sso/i });
    expect(screen.getByText("or")).toBeInTheDocument();
    const submit = screen.getByRole("button", { name: "Sign in" });
    expect(submit.className).toContain("bg-primary");
    expect(ssoLink.className).not.toContain("bg-primary");
  });

  it("both bits false keeps the admin-token form (nothing says sign-in is unavailable), no SSO control at all", async () => {
    healthMock.mockResolvedValue({ sso: false, sso_only: false, token_login: false });
    renderSignIn();
    expect(await screen.findByLabelText(/admin token/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /sign in with sso/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /sign in with sso/i })).not.toBeInTheDocument();
  });

  it("local-mode cell: an older/local daemon reporting no posture bits keeps today's form, no SSO control", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
    renderSignIn();
    expect(await screen.findByLabelText(/admin token/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /sign in with sso/i })).not.toBeInTheDocument();
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
  });

  // R4/F027's failure shape, replayed for the two new bits: health() resolves
  // the EMPTY object on a network error or any non-2xx, and the gate must
  // leave its LAST KNOWN state alone rather than read "no answer" as "nothing
  // works". #457: while the daemon has NEVER answered, the honest read is the
  // checking state (below), not a guessed-at form — this pins the OTHER half:
  // once a real answer has landed, a LATER failed read must not blank it out.
  it("a failed /healthz AFTER a real answer does not hide the admin-token form or misreport the posture", async () => {
    vi.useFakeTimers();
    try {
      healthMock.mockReset();
      healthMock.mockResolvedValueOnce({ status: "ok" }).mockResolvedValue({});
      renderSignIn();
      await act(async () => void (await vi.advanceTimersByTimeAsync(0)));
      expect(screen.getByLabelText(/admin token/i)).toBeInTheDocument();

      // A later read that comes back empty (outage) must not blank the form
      // the daemon already answered for.
      await act(async () => void (await vi.advanceTimersByTimeAsync(10_000)));
      expect(screen.getByLabelText(/admin token/i)).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: /sign in with sso/i })).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
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
    healthMock.mockResolvedValue({ status: "ok" });
    renderSignIn();
    expect(await screen.findByText(SIGNIN.TOKEN_HINT)).toBeInTheDocument();
    expect(screen.queryByText(/WARDYN_ADMIN_TOKEN/)).not.toBeInTheDocument();
    expect(screen.queryByText(/demo-admin-token/)).not.toBeInTheDocument();
  });

  it("the token input's placeholder carries no working demo credential", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
    renderSignIn();
    const input = await screen.findByLabelText(/admin token/i);
    expect(input).not.toHaveAttribute("placeholder", "demo-admin-token");
    expect(input.getAttribute("placeholder") ?? "").toBe("");
  });
});

// Every submitToken failure used to collapse to probeAuth's plain
// boolean, so a daemon 5xx and an unreachable control plane both rendered the
// SAME "That admin token was rejected" copy as an actually-bad token — and
// cleared a token that may have been perfectly valid.
describe("SignIn — submitToken tells a rejected token apart from a reachability failure", () => {
  afterEach(() => vi.unstubAllGlobals());

  async function submit(token = "sometoken") {
    sessionStorage.clear();
    localStorage.clear();
    healthMock.mockResolvedValue({ status: "ok" });
    renderSignIn();
    await userEvent.type(await screen.findByLabelText(/admin token/i), token);
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

  it("a network error (daemon down / unreachable) shows a reachability message naming Wardyn, not the rejected-token copy (#212)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to fetch")));
    await submit();
    expect(await screen.findByText(SIGNIN.UNREACHABLE_ERROR)).toBeInTheDocument();
    expect(screen.queryByText(/that admin token was rejected/i)).not.toBeInTheDocument();
    expect(getToken()).toBe("sometoken");
  });
});

// Re-fix: the OIDC callback redirects a user-actionable login
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
    // #457: name "your Wardyn admin" as who to ask, no env var — a signed-in
    // human should never be told to go set a chart value themselves.
    expect(alert).toHaveTextContent(/ask your wardyn admin/i);
    expect(alert).not.toHaveTextContent(/WARDYN_/);
    expect(window.location.search).toBe("");
  });

  it("renders the email_domain message, pointing this locked-out reader at their admin rather than an env var they cannot reach (#212)", async () => {
    window.history.pushState({}, "", "/?auth_error=email_domain");
    healthMock.mockResolvedValue({});
    renderSignIn();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(SIGNIN.EMAIL_DOMAIN);
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
  // the real remedy (App Role or group), not domains, and (#457) no env var.
  it("renders the email_verified_absent message, distinct from email_unverified", async () => {
    window.history.pushState({}, "", "/?auth_error=email_verified_absent");
    healthMock.mockResolvedValue({});
    renderSignIn();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/doesn't send an email_verified claim/i);
    expect(alert).not.toHaveTextContent(/WARDYN_/);
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
    expect(alert).not.toHaveTextContent(/WARDYN_/);
  });

  it("falls back to a generic message for an unrecognized code, naming your Wardyn admin (#457)", async () => {
    window.history.pushState({}, "", "/?auth_error=something_new");
    healthMock.mockResolvedValue({});
    renderSignIn();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(SIGNIN.AUTH_FAILED);
    expect(alert).not.toHaveTextContent(/operator/i);
  });

  it("renders the oidc_config message, no env var, pointed at your Wardyn admin (#457)", async () => {
    window.history.pushState({}, "", "/?auth_error=oidc_config");
    healthMock.mockResolvedValue({});
    renderSignIn();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(SIGNIN.OIDC_CONFIG);
    expect(alert).not.toHaveTextContent(/WARDYN_/);
  });

  it("renders the oidc_transient message unchanged", async () => {
    window.history.pushState({}, "", "/?auth_error=oidc_transient");
    healthMock.mockResolvedValue({});
    renderSignIn();
    expect(await screen.findByRole("alert")).toHaveTextContent(SIGNIN.OIDC_TRANSIENT);
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

// #457: no user-visible sign-in/states string names an env var (jargon this
// screen's readers — some not even signed in — cannot act on) or says
// "Please" (no other refusal in the console does).
describe("SignIn — no env var names, no 'Please' (#457)", () => {
  it("no SIGNIN string contains WARDYN_ or 'Please'", () => {
    for (const value of Object.values(SIGNIN)) {
      expect(value).not.toMatch(/WARDYN_/);
      expect(value).not.toMatch(/Please/);
    }
  });

  it("no STATES string contains WARDYN_ or 'Please'", () => {
    for (const value of Object.values(STATES)) {
      expect(value).not.toMatch(/WARDYN_/);
      expect(value).not.toMatch(/Please/);
    }
  });
});
