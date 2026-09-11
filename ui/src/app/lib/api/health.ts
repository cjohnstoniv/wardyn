/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Health/liveness, session (logout/whoami), and the operator-wide site config.
// The small "server & session" surface the shell always needs.
import { SERVER_OWNED_SITE_CONFIG_KEYS, type SiteConfig } from "../types";
import type { DriveBackend, StorageEnforcement } from "./drives";
import { WFETCH_TIMEOUT_MS, asJson, wfetch } from "./core";

// GET /me's `user_drive` (0.7, migration 0054) — what this caller would mount
// if they asked for it on their next run, or null when they would mount
// nothing. Mirrors internal/api/user_drives.go's meUserDrive 1:1, omitempty
// included: an absent size_mib is "no allocation shown" (the copy module's
// _NOSIZE twins), an absent paused is false.
//
// It carries NO DOOR FIELD, deliberately: the door is a property of the
// caller's PROFILE, not of the allocation, and rides beside it as
// Me.user_drive_denied_by_profile. Folding them would make the two states the
// member surfaces exist to tell apart — "denied AND unallocated" vs "simply
// unallocated" — inexpressible.
export interface MeUserDrive {
  name: string;
  // The registry's own two unions (api/drives.ts), not a second spelling of
  // them: /me answers with the very backend and enforcement the /drives screen
  // registered, so a fifth backend added there must not typecheck here until
  // this surface has been thought about.
  backend: DriveBackend;
  /** Omitted (or 0) means no allocation is shown, never "zero bytes". */
  size_mib?: number;
  writable: boolean;
  enforcement: StorageEnforcement;
  home_name?: string;
  /** Allocated, then disabled by an admin. Omitted when false. */
  paused?: boolean;
}

// GET /me — see whoami() below for where each field comes from. ONE
// declaration rather than the two identical inline literals this used to
// carry: a field added to the promise type and forgotten in the response cast
// is a field the console silently cannot read.
export interface Me {
  principal: string;
  method: string;
  operator: boolean;
  security_operator: boolean;
  role: "admin" | "security_admin" | "member";
  email: string;
  // The IdP's display-name claim — "" outside SSO or when the IdP sent none,
  // absent on a pre-0.7.1 daemon. Display only: the header reads name, then
  // email, then principal; `principal` stays the ownership key.
  name?: string;
  // ISO timestamp the SSO session dies at, with no refresh (W31-S1-7) —
  // present only for method:"sso". Absent for local/token auth, which has no
  // session to expire.
  session_expires_at?: string;
  // M3 — presentational label of this member's WARDYN_MEMBER_WORKSPACE_ROOTS
  // /_MAP constraint (e.g. "/home/agent-projects"). null/absent when no root
  // applies (member-role-desktop.md §DECISIONS O1). Never a value to trust —
  // AddWorkspaceDialog shows it as a hint; ValidateMemberMountSource enforces.
  member_local_dir_root?: string | null;
  // The caller's own user drive, null-means-no-allocation — see MeUserDrive.
  // Absent on a pre-0.7 daemon, which reads the same as "none".
  user_drive?: MeUserDrive | null;
  // THE DOOR: the governance profile's NAME when its DenyUserDrive limit
  // refuses this caller a drive, "" when it does not. Always present on a 0.7
  // daemon (never omitted), so an absent key is an older server rather than an
  // open door. Non-empty is the whole bit — and it renders even when
  // user_drive is null, the state where "ask an admin for an allocation" is
  // the wrong advice.
  user_drive_denied_by_profile?: string;
  // WHY /me COULD NOT ANSWER for this caller's drive, or "" when it could.
  // Always present on a 0.7 daemon, so an absent key is an older server rather
  // than "nothing is wrong".
  //
  // It exists because `user_drive: null` means "you have no allocation", which
  // is ADVICE — and three server states the LAUNCH path refuses with 403, 422
  // and 500 used to wear that same answer. A closed set, matching the server's
  // own vocabulary beside writeDriveError: "groups_snapshot_stale" (sign in
  // again), "unmountable" (an allocation exists and an admin must fix its
  // directory name), "unavailable" (the allocation could not be read), and
  // "governance_unavailable" (the ceiling could not be read, so whether the
  // door is open is unknown — the allocation is withheld with it).
  //
  // Non-empty means the drive affordance must NOT be offered: the server has
  // not said the mount would work. Rendering the remedy in its place is a copy
  // change and is not wired yet — the card falls back to offering nothing,
  // which is what it already did for all four states.
  user_drive_unavailable?: string;
}

// Result of a real throwaway-sandbox probe (test-proxy / test-redirect) —
// never a cached or inferred verdict (T.TEST_STANDING). "bypass" is the
// redirect-only case where the mirror answered but the public endpoint is
// STILL reachable from a sandbox (T.TEST_BYPASS); "no_runner" means there's
// nothing on this host to launch the probe with (T.TEST_NORUNNER), including
// an older server with no test endpoint at all. "not_run" means a runner IS
// configured, but the throwaway sandbox that would have carried the probe
// never got to running it (e.g. an image pull failure, or a confinement
// class this host can't enforce) — nothing was learned about the network
// either way, so it must never render as blocked (T.GATE_HEAD_NOT_RUN).
// "timed_out" means the sandbox started and ran but the run never reported
// completion within the budget — the detail names the sandbox's state at the
// deadline; a runner/recording-upload fact, never a network verdict.
export interface ProxyTestResult {
  state: "reached" | "blocked" | "bypass" | "no_runner" | "not_run" | "timed_out";
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
  /** state=reached only: the probe passed, but this run's session recording
   *  never reached the control plane — runs will complete, but recordings
   *  will be lost. Absent on a clean pass; never changes the gate (reached
   *  still unlocks Next either way). */
  warning?: string;
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
  //
  // R4/F029: `integrations` was stripped by NAME, so the SECOND server-owned
  // field added to the same document (onboarding_completed_at) repeated the
  // bug verbatim — every Corporate-network save 400'd once onboarding had
  // completed. The strip is now driven by SERVER_OWNED_SITE_CONFIG_KEYS
  // (lib/types/site.ts), the one list a third such field gets added to.
  async putSiteConfig(cfg: SiteConfig): Promise<void> {
    const body: Record<string, unknown> = { ...cfg };
    for (const k of SERVER_OWNED_SITE_CONFIG_KEYS) delete body[k];
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
    // "ok" iff the daemon actually answered — the {} returned below on a
    // network error or a non-2xx has none, which is how the shell's heartbeat
    // (App.tsx) reads "control plane unreachable".
    status?: string;
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
    // UI-sandbox gateway discovery (run-detail's "UI apps" lane): absent when
    // the gateway is off (WARDYN_UI_SANDBOX_LISTEN unset) or an older daemon —
    // both read as "no lane", never a false-enabled guess. enter_url_template
    // is the ONE field the console reads to build the open URL — it never
    // composes the UI origin itself, only substitutes {run}/{app}/{ticket}
    // (internal/api/uigateway.go's uiSandboxHealthz).
    ui_sandbox?: { enabled?: boolean; enter_url_template?: string; host_mode?: boolean };
    // Per-pluggable-seam selection (server.go's ComponentInfo), keyed by seam
    // name ("recording", "identity", ...). W21-S1-7: recording.selected ===
    // "none" is the honest signal that THIS deployment's recording store
    // never came up (stock Helm install: persistence off) — distinct from
    // "no run has produced one yet". Absent on an older daemon.
    components?: Record<string, { selected?: string; available?: string[]; source?: string }>;
  }> {
    try {
      // /healthz is un-prefixed (not under /api/v1), so it is the ONE call
      // that cannot go through wfetch — and therefore the one that has to
      // repeat its deadline. Without it a hung /healthz alone strands the
      // shell: useMeta only sets resolved:true once health() SETTLES, and
      // every route is gated behind that (app-shell.tsx:128-158, App.tsx's
      // roleResolved). The catch below already turns a failure into {}, which
      // is exactly how the shell reads "control plane unreachable".
      const res = await fetch("/healthz", {
        credentials: "include",
        signal: AbortSignal.timeout(WFETCH_TIMEOUT_MS),
      });
      if (!res.ok) return {};
      return (await res.json()) as Record<string, unknown>;
    } catch {
      return {};
    }
  },

  // GET /readyz — READINESS, which is a different question from the liveness
  // /healthz answers, and R4/F066 is what it cost to read only the first one.
  //
  // handleHealthz emits `"status": "ok"` as a LITERAL (internal/api/healthz.go)
  // — it never touches the store, deliberately: liveness must not restart a pod
  // because Postgres failed over. So with wardynd up and the store down,
  // /healthz says "ok", the console's unreachable banner stays down, and every
  // polled screen keeps rendering last-good data behind its own silent
  // `.catch` — a live-looking cockpit frozen at the moment the store died,
  // which is the EXACT state App.tsx's own comment says the banner exists to
  // prevent ("without this reads exactly like a healthy quiet fleet").
  //
  // /readyz is the endpoint that already asks: it Pings the store under a 3s
  // timeout and answers 503 {"status":"error","postgres":"unreachable"}
  // (internal/api/security_headers.go:124-136). Anonymous, like /healthz
  // (routes.go:38), so the gate can read it too.
  //
  // Same {}-on-no-answer contract as health() above: a 503, a non-JSON body and
  // a dead network all resolve to an object with no `status`, so a missing
  // status:"ok" IS the not-ready verdict and no caller has to catch.
  async readyz(): Promise<{ status?: string; postgres?: string }> {
    try {
      const res = await fetch("/readyz", { credentials: "include" });
      if (!res.ok) return {};
      return (await res.json()) as { status?: string; postgres?: string };
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
  // directly — three-valued since 0.7 ("admin"/"security_admin"/"member");
  // `email` is the OIDC claim (empty outside SSO).
  //
  // `security_operator` is the SECOND predicate (isSecurityOperator): admin OR
  // security_admin, gating the security-governance surfaces (approvals, audit,
  // permissions, governance profiles). `operator` deliberately stays
  // super-admin-only — the two are NOT complementary now that role has three
  // values, so gate each control on the one that matches its route.
  // `user_drive` / `user_drive_denied_by_profile` are the 0.7 member-drive
  // pair — see MeUserDrive above for why the door is a sibling field and not
  // a property of the allocation.
  async whoami(): Promise<Me | null> {
    try {
      const res = await wfetch("/me", { method: "GET" });
      if (!res.ok) return null;
      return (await res.json()) as Me;
    } catch {
      return null;
    }
  },
};
