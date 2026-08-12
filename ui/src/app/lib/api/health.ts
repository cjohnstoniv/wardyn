/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Health/liveness, session (logout/whoami), and the operator-wide site config.
// The small "server & session" surface the shell always needs.
import type { SiteConfig } from "../types";
import { asJson, wfetch } from "./core";

// Result of a real throwaway-sandbox probe (test-proxy / test-redirect) —
// never a cached or inferred verdict (T.TEST_STANDING). "bypass" is the
// redirect-only case where the mirror answered but the public endpoint is
// STILL reachable from a sandbox (T.TEST_BYPASS); "no_runner" means there's
// nothing on this host to launch the probe with (T.TEST_NORUNNER), including
// an older server with no test endpoint at all.
export interface ProxyTestResult {
  state: "reached" | "blocked" | "bypass" | "no_runner";
  detail: string;
  elapsed_ms?: number;
  /** Which path the proxy probe actually traversed. test-proxy only; absent
   *  from redirect probes and older servers. */
  via?: "proxy" | "direct";
  /** state=blocked's captive-portal flavor: something ANSWERED, but not with
   *  the endpoint's published payload (T.TEST_INTERCEPTED's case). Same
   *  verdict as blocked, rendered apart — it sends the operator to a
   *  different person than a refused connection does. */
  intercepted?: boolean;
  /** The probe hit a caller-named URL with no known payload to verify — a
   *  reached here is the deliberately WEAKER "request completed" claim
   *  (T.CUSTOM_CAVEAT), never the builtin targets' "payloads matched". */
  custom?: boolean;
}

export const health = {
  // GET /api/v1/site-config — the operator-wide baseline (upstream proxy secret
  // ref / per-ecosystem artifact-registry overrides / default SCM hosts). An
  // operator who has never configured one gets the zero value ({}), not a 404 —
  // the backend treats "unconfigured" as a valid, common state; the 404 check
  // here is just defensive for an older/mid-rollout backend.
  async getSiteConfig(): Promise<SiteConfig> {
    const res = await wfetch("/site-config", { method: "GET" });
    if (res.status === 404) return {};
    return asJson<SiteConfig>(res);
  },

  // PUT /api/v1/site-config — REPLACES the whole document; callers must GET
  // first and spread onto the current value to avoid clobbering fields they
  // don't intend to change. Admin-gated server-side: a non-admin human gets a
  // 403, which surfaces here as an HttpError (via asJson) like any other write.
  //
  // HIGH fix: every writer in this codebase gets its starting `cfg` from a
  // GET (see SiteConfig.integrations' doc comment) and spreads onto it, so a
  // stored integration would otherwise ride along into the PUT body and hit
  // the server's hard 400 ("integrations are managed through their own
  // endpoints, not PUT /site-config") on every save. Strip it here, once, so
  // no caller has to remember to.
  async putSiteConfig(cfg: SiteConfig): Promise<void> {
    const { integrations: _integrations, ...body } = cfg;
    const res = await wfetch("/site-config", { method: "PUT", body: JSON.stringify(body) });
    await asJson<SiteConfig>(res);
  },

  // POST /api/v1/site-config/test-proxy — launches a throwaway confined probe
  // through wardyn-proxy chained to the configured upstream and reports what
  // actually happened (T.TEST_PROXY_HINT); never a cached/inferred verdict. A
  // 404 means this server build predates the endpoint — that must read as
  // "can't test here" (no_runner), never a fake pass.
  //
  // url is the deliberate escape for a host with no public internet — an
  // internal-only or air-gapped deployment points the probe at something it
  // CAN reach instead of failing the default multi-target check forever. Omit
  // it (the ordinary case) to run the default check; a rejected custom URL
  // comes back as a 400 (asJson throws), surfaced by the caller like any other
  // write failure — never papered over.
  async testProxy(url?: string): Promise<ProxyTestResult> {
    const res = await wfetch("/site-config/test-proxy", {
      method: "POST",
      ...(url ? { body: JSON.stringify({ url }) } : {}),
    });
    if (res.status === 404) {
      return { state: "no_runner", detail: "This server build has no test-proxy endpoint." };
    }
    return asJson<ProxyTestResult>(res);
  },

  // POST /api/v1/site-config/test-redirect — the same real probe, for one
  // egress redirect. A redirect has no server-side id, so it's identified by
  // its from/to pair (what the row already holds client-side).
  async testRedirect(from: string, to: string): Promise<ProxyTestResult> {
    const res = await wfetch("/site-config/test-redirect", {
      method: "POST",
      body: JSON.stringify({ from, to }),
    });
    if (res.status === 404) {
      return { state: "no_runner", detail: "This server build has no test-redirect endpoint." };
    }
    return asJson<ProxyTestResult>(res);
  },

  // GET /healthz — liveness + trust boundary (unauthenticated; surfaced in the
  // shell so the real trust domain / identity provider are always visible).
  // ebpf_groundtruth is the kernel ground-truth stream's honest health
  // (server.go ebpfGroundtruthStatus): unavailable = no sensor has ever beaten,
  // degraded = stale heartbeat, idle = beating but blind, healthy = fresh beats
  // AND real kernel events. Absent on an older daemon.
  async health(): Promise<{
    trust_domain?: string;
    identity_provider?: string;
    runner?: string;
    confinement_classes?: string[];
    ebpf_groundtruth?: { state?: string; reason?: string };
    // OIDC is configured, so GET /auth/login exists — the sign-in screen only
    // offers the SSO link when the server says the flow is actually mounted.
    sso?: boolean;
    // SSH gateway discovery (run-detail's "Connect via SSH" pane): absent /
    // undefined on a deployment with the gateway off (WARDYN_SSH_LISTEN
    // unset) or an older daemon — both must read as "no pane", never a
    // false-enabled guess. advertise_addr / host_key_fingerprint are both
    // non-secret (see docs/SSH.md) — the fingerprint is public by design.
    ssh?: { enabled?: boolean; advertise_addr?: string; host_key_fingerprint?: string };
  }> {
    try {
      const res = await fetch("/healthz", { credentials: "include" });
      if (!res.ok) return {};
      return (await res.json()) as Record<string, unknown>;
    } catch {
      return {};
    }
  },

  // POST /api/v1/auth/logout — terminate the server-side OIDC session.
  //
  // HIGH fix (sign-out): clearing the local admin token is not enough — the
  // OIDC session cookie is HttpOnly and lives on the server, so without this
  // call the very next auth probe (which sends the cookie) silently re-signs the
  // operator back in. We MUST tell the server to clear the session. The cookie
  // is sent via credentials:"include". Best-effort: a failed logout (server
  // error / network down) still resolves so the client can fall back to the
  // sign-in gate; we never want a hung spinner blocking sign-out.
  async logout(): Promise<void> {
    // FIX #6: do NOT silently swallow a failed logout — a non-OK response or a
    // network error means the server-side OIDC session may STILL be valid, so the
    // operator only *believes* they signed out. Surface it (console.error) while
    // still resolving, so the caller can fall back to the sign-in gate without a
    // hung spinner, but a failed sign-out is never invisible.
    try {
      const res = await wfetch("/auth/logout", { method: "POST" });
      if (!res.ok) {
        console.error(`logout: server returned HTTP ${res.status}; session may still be active`);
      }
    } catch (err) {
      console.error("logout: request failed; session may still be active", err);
    }
  },

  // GET /api/v1/me — the authenticated principal + auth method + role.
  // `operator` comes from the SAME predicate (isOperator) that gates the 25
  // operator-only routes server-side (internal/api/http.go) — never a second,
  // driftable copy of the rule. A viewer (operator:false) reads everything but
  // is refused on writes; see wardyn/operator-context.tsx for how the console
  // uses this to disable those controls instead of letting a viewer discover
  // the tier as a raw 403. `role` (B3) is the same B1-derived tier named
  // directly ("admin"/"member" — admin === operator); `email` is the OIDC
  // claim (empty outside SSO).
  async whoami(): Promise<{
    principal: string;
    method: string;
    operator: boolean;
    role: "admin" | "member";
    email: string;
  } | null> {
    try {
      const res = await wfetch("/me", { method: "GET" });
      if (!res.ok) return null;
      return (await res.json()) as {
        principal: string;
        method: string;
        operator: boolean;
        role: "admin" | "member";
        email: string;
      };
    } catch {
      return null;
    }
  },
};
