/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
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
