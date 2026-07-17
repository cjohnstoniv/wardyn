/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Health/liveness, session (logout/whoami), and the operator-wide site config.
// The small "server & session" surface the shell always needs.
import type { SiteConfig } from "../types";
import { asJson, wfetch } from "./core";

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
  async putSiteConfig(cfg: SiteConfig): Promise<void> {
    const res = await wfetch("/site-config", { method: "PUT", body: JSON.stringify(cfg) });
    await asJson<SiteConfig>(res);
  },

  // GET /healthz — liveness + trust boundary (unauthenticated; surfaced in the
  // shell so the real trust domain / identity provider are always visible).
  async health(): Promise<{ trust_domain?: string; identity_provider?: string; runner?: string; confinement_classes?: string[] }> {
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

  // GET /api/v1/me — the authenticated principal + auth method.
  async whoami(): Promise<{ principal: string; method: string } | null> {
    try {
      const res = await wfetch("/me", { method: "GET" });
      if (!res.ok) return null;
      return (await res.json()) as { principal: string; method: string };
    } catch {
      return null;
    }
  },
};
