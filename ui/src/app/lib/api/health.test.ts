/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { health } from "./health";
import { SERVER_OWNED_SITE_CONFIG_KEYS } from "../types";

// Regression for the sign-out HIGH finding: signing out only cleared the local
// admin token, never calling the server logout endpoint, so the OIDC session
// cookie survived and the next auth probe silently re-signed-in. logout()
// must hit /api/v1/auth/logout with the cookie attached (credentials:include),
// so the server can clear the session.
describe("health.logout()", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("calls the auth/logout endpoint with credentials included", async () => {
    await health.logout();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/v1/auth/logout");
    expect(init?.credentials).toBe("include");
  });

  it("resolves even when the server returns an error (best-effort logout)", async () => {
    fetchMock.mockResolvedValueOnce(new Response("boom", { status: 500 }));
    await expect(health.logout()).resolves.toBeUndefined();
  });

  it("resolves even when the network throws (best-effort logout)", async () => {
    fetchMock.mockRejectedValueOnce(new TypeError("network down"));
    await expect(health.logout()).resolves.toBeUndefined();
  });
});

// HIGH fix: GET /site-config echoes the stored `integrations` array, and every
// writer in this codebase (setup-screen.tsx, add-integration-dialog.tsx)
// spreads that GET response straight into a PUT body to patch one field — but
// the server hard-400s any PUT carrying a non-empty `integrations`
// (internal/api/site_config.go's handlePutSiteConfig), so every save broke as
// soon as one integration was stored. Pin the round-trip: whatever GET
// returns, the PUT body must never carry `integrations`.
describe("health — site-config integrations round-trip", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("strips integrations from a GET response spread straight into putSiteConfig", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ scm_hosts: ["github.com"], integrations: [{ id: "anthropic_api_key" }] }), {
        status: 200,
      }),
    );
    const got = await health.getSiteConfig();
    expect(got.integrations).toEqual([{ id: "anthropic_api_key" }]); // sanity: GET really carried it

    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({}), { status: 200 }));
    // The exact "patch one field" idiom every real caller uses.
    await health.putSiteConfig({ ...got, scm_hosts: ["github.com", "gitlab.com"] });

    expect(fetchMock).toHaveBeenCalledTimes(2);
    const [, putInit] = fetchMock.mock.calls[1];
    const body = JSON.parse(putInit!.body as string);
    expect(body).not.toHaveProperty("integrations");
    expect(body.scm_hosts).toEqual(["github.com", "gitlab.com"]);
  });

  // R4/F029: the SECOND server-owned key on the same document repeated the
  // first one's bug because the strip was written by name. Derived from
  // SERVER_OWNED_SITE_CONFIG_KEYS so a THIRD one cannot ship unstripped.
  it("strips EVERY server-owned key a GET echoes, not just integrations", async () => {
    const echoed: Record<string, unknown> = {
      scm_hosts: ["github.com"],
      integrations: [{ id: "anthropic_api_key" }],
      onboarding_completed_at: "2026-08-01T00:00:00Z",
    };
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify(echoed), { status: 200 }));
    const got = await health.getSiteConfig();

    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({}), { status: 200 }));
    // corp-network-proxy.tsx:566's exact idiom: `{ ...(siteConfig ?? {}), field }`.
    await health.putSiteConfig({ ...got, upstream_proxy_url: "http://proxy.corp.example:3128" });

    const [, putInit] = fetchMock.mock.calls[1];
    const body = JSON.parse(putInit!.body as string);
    // The key the by-name strip missed: the server 400s the whole save on it.
    expect(body).not.toHaveProperty("onboarding_completed_at");
    expect(body).not.toHaveProperty("integrations");
    // ...and derived, so a THIRD server-owned field cannot ship unstripped.
    for (const k of SERVER_OWNED_SITE_CONFIG_KEYS) {
      expect(echoed).toHaveProperty(k); // sanity: the fixture really carries each one
      expect(body).not.toHaveProperty(k);
    }
    expect(body.upstream_proxy_url).toBe("http://proxy.corp.example:3128");
    expect(body.scm_hosts).toEqual(["github.com"]);
  });
});

// testProxy's optional url — the escape for a host with no public internet
// (see health.ts's doc comment). No url must keep sending a bare POST (the
// default multi-target check); a url must be the ONLY thing in the body.
describe("health.testProxy(url?)", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ state: "reached", detail: "ok" }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("sends no body when called with no url", async () => {
    await health.testProxy();
    const [, init] = fetchMock.mock.calls[0];
    expect(init?.body).toBeUndefined();
  });

  it("sends {url} when called with a custom url, and nothing else", async () => {
    await health.testProxy("https://intranet.example.com");
    const [, init] = fetchMock.mock.calls[0];
    expect(JSON.parse(init!.body as string)).toEqual({ url: "https://intranet.example.com" });
  });
});

// /me's 0.7 user-drive pair. The two fields are INDEPENDENT on the wire
// (internal/api/me.go writes them as siblings) because there are four states
// and one field carries three — so the round-trip has to prove both survive,
// including the one a folded field could not express: a shut door with no
// allocation, where "ask an admin for an allocation" is the wrong advice.
describe("health.whoami() — the user-drive pair", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  const meBody = (extra: Record<string, unknown>) => ({
    principal: "alice@corp.example",
    method: "sso",
    operator: false,
    security_operator: false,
    role: "member",
    email: "alice@corp.example",
    ...extra,
  });

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  const answer = (body: unknown) =>
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(body), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    );

  it("reads an allocation whole", async () => {
    answer(
      meBody({
        user_drive: {
          name: "Scratch",
          backend: "k8s_pvc",
          size_mib: 16384,
          writable: true,
          enforcement: "request",
          home_name: "d-3f9a1c7e",
        },
        user_drive_denied_by_profile: "",
      }),
    );
    const me = await health.whoami();
    expect(me?.user_drive).toEqual({
      name: "Scratch",
      backend: "k8s_pvc",
      size_mib: 16384,
      writable: true,
      enforcement: "request",
      home_name: "d-3f9a1c7e",
    });
    expect(me?.user_drive_denied_by_profile).toBe("");
  });

  it("reads a paused allocation as an allocation, not as nothing", async () => {
    answer(
      meBody({
        user_drive: {
          name: "Scratch",
          backend: "docker_volume",
          writable: true,
          enforcement: "none",
          paused: true,
        },
        user_drive_denied_by_profile: "",
      }),
    );
    const me = await health.whoami();
    expect(me?.user_drive?.paused).toBe(true);
    // omitempty: no size on the wire is "no allocation shown", never 0 bytes.
    expect(me?.user_drive?.size_mib).toBeUndefined();
  });

  it("carries a shut door with NO allocation — the state a folded field could not express", async () => {
    answer(meBody({ user_drive: null, user_drive_denied_by_profile: "Greenfield contractors" }));
    const me = await health.whoami();
    expect(me?.user_drive).toBeNull();
    expect(me?.user_drive_denied_by_profile).toBe("Greenfield contractors");
  });

  it("leaves both absent on a pre-0.7 daemon — undefined, not a guessed default", async () => {
    answer(meBody({}));
    const me = await health.whoami();
    expect(me?.user_drive).toBeUndefined();
    expect(me?.user_drive_denied_by_profile).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// R4/F066 — the console's reachability oracle has to be able to SEE a store
// outage. /healthz cannot: handleHealthz writes `"status": "ok"` as a literal
// and never touches the store (internal/api/healthz.go), on purpose — liveness
// must not restart a pod because Postgres failed over. /readyz is the probe
// that already Pings it (internal/api/security_headers.go:124-136), and this
// reader gives it the same {}-on-no-answer contract health() has, so App.tsx
// can treat "not ready" and "not live" as one verdict without a catch.
// ---------------------------------------------------------------------------
describe("health.readyz — the store probe /healthz deliberately isn't", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  it("asks /readyz, not /healthz", async () => {
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
    await health.readyz();
    expect(String(fetchMock.mock.calls[0][0])).toBe("/readyz");
  });

  it("a ready daemon answers status ok", async () => {
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
    expect(await health.readyz()).toEqual({ status: "ok" });
  });

  // THE case: wardynd up, Postgres down. handleReadyz answers 503 with
  // {"status":"error","postgres":"unreachable"} — the reader must not turn that
  // into an "ok" the banner would then miss, and must not reject either (every
  // caller of this is a background poll).
  it("a 503 store outage resolves to NO status — never a quiet ok, never a rejection", async () => {
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ status: "error", postgres: "unreachable" }), { status: 503 }),
    );
    const r = await health.readyz();
    expect(r.status).toBeUndefined();
    expect(r.status === "ok").toBe(false);
  });

  it("a dead network resolves to NO status rather than rejecting", async () => {
    fetchMock.mockRejectedValue(new TypeError("Failed to fetch"));
    expect(await health.readyz()).toEqual({});
  });

  it("an older daemon with no /readyz route (404) is not-ready, not ok", async () => {
    fetchMock.mockResolvedValue(new Response("404 page not found", { status: 404 }));
    expect((await health.readyz()).status).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// R4/F010 — SOURCE PARITY for the GET /me wire mirror.
//
// The sibling run-wire mirror is pinned MECHANICALLY (runs.wire.fields.test.ts
// :280-343: it readFileSync's the Go source, extracts the json tags, and fails
// on drift — "A Go rename ... fails HERE instead of as a runtime `undefined`").
// GET /me had no such gate, and every test that asserts on its keys — this
// file, app-shell.test.tsx, new-run-screen.test.tsx, member-getting-started
// .test.tsx — builds the body from a hand-written literal, so none of them can
// observe the Go side at all.
//
// The cost of that blind spot is not cosmetic: app-shell.tsx reads these keys
// FAIL-OPEN (`me?.operator ?? true`, `me?.security_operator ?? true`,
// `me?.role ?? "admin"`), so a renamed or dropped Go key resolves to undefined
// and every console gate OPENS rather than closing. A rename must fail here.
//
// handleMe composes its body as a map literal plus `body["…"] = …` assignments
// rather than a tagged struct, so the keys are read from those two shapes; the
// drive sub-object IS a struct (meUserDrive) and is read from its json tags.
// ---------------------------------------------------------------------------

// Walk up to go.mod so the file works from ui/ (vitest's cwd) or anywhere under
// it — the same discipline as runs.wire.fields.test.ts:246-253.
function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

// The source of ONE top-level Go func, from its signature to the closing brace
// in column 0 (every nested block in these handlers is indented, so the first
// "\n}\n" is the end of the function).
function goFuncSource(src: string, signature: string): string {
  const at = src.indexOf(signature);
  if (at < 0) throw new Error(`func not found: ${signature}`);
  const end = src.indexOf("\n}\n", at);
  if (end < 0) throw new Error(`func has no column-0 closing brace: ${signature}`);
  return src.slice(at, end);
}

// Every response key a handler writes onto `<mapVar>`: the `"key": value` pairs
// of the map literal plus the later `<mapVar>["key"] = …` assignments. Comment
// lines are dropped first so prose that quotes a key name cannot count as one.
function goResponseKeys(fnSrc: string, mapVar: string): string[] {
  const code = fnSrc
    .split("\n")
    .filter((l) => !/^\s*\/\//.test(l))
    .join("\n");
  const keys = new Set<string>();
  for (const m of code.matchAll(/"([a-z0-9_]+)":\s/g)) keys.add(m[1]);
  for (const m of code.matchAll(new RegExp(`\\b${mapVar}\\["([a-z0-9_]+)"\\]\\s*=`, "g"))) keys.add(m[1]);
  return [...keys].sort();
}

// The json tag names of one Go struct body (runs.wire.fields.test.ts:258-266).
function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  const tags: string[] = [];
  for (const t of m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)) {
    if (t[1] !== "-") tags.push(t[1]);
  }
  return tags;
}

// The property names of one TS interface body (runs.wire.fields.test.ts:269-278).
function tsInterfaceKeys(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`interface ${name} not found`);
  const keys: string[] = [];
  for (const line of m[1].split("\n")) {
    const k = /^\s*(?:readonly\s+)?([A-Za-z_][A-Za-z0-9_]*)\??\s*:/.exec(line);
    if (k) keys.push(k[1]);
  }
  return keys;
}

describe("source parity — GET /me's Go body vs the TS Me mirror (F010)", () => {
  const root = repoRoot();
  const meGo = readFileSync(join(root, "internal/api/me.go"), "utf8");
  const drivesGo = readFileSync(join(root, "internal/api/user_drives.go"), "utf8");
  const healthTs = readFileSync(join(root, "ui/src/app/lib/api/health.ts"), "utf8");

  it("every key handleMe writes is declared on Me, and every Me key is one handleMe writes", () => {
    const goKeys = goResponseKeys(goFuncSource(meGo, "func (s *Server) handleMe("), "body").sort();
    // Stale-regex guard: the extractor silently returning nothing must not read
    // as parity (runs.wire.fields.test.ts does the same with its own floor).
    expect(goKeys.length).toBeGreaterThanOrEqual(10);
    const tsKeys = tsInterfaceKeys(healthTs, "Me").sort();
    expect(tsKeys.length).toBeGreaterThanOrEqual(10);
    expect(
      goKeys.filter((k) => !tsKeys.includes(k)),
      "handleMe answers these and the TS Me mirror cannot read them — add them to `Me` " +
        "(health.ts) with the Go line cited, per the TS-mirror law",
    ).toEqual([]);
    expect(
      tsKeys.filter((k) => !goKeys.includes(k)),
      "the console reads these off /me but handleMe (internal/api/me.go) never writes them — " +
        "a Go rename/drop, which app-shell.tsx resolves to its FAIL-OPEN default " +
        "(`me?.operator ?? true`, `me?.role ?? \"admin\"`) rather than to a closed gate",
    ).toEqual([]);
  });

  it("meUserDrive's json tags are exactly the MeUserDrive mirror's keys", () => {
    const goTags = goJSONTags(drivesGo, "meUserDrive").sort();
    expect(goTags.length).toBeGreaterThanOrEqual(6);
    const tsKeys = tsInterfaceKeys(healthTs, "MeUserDrive").sort();
    expect(tsKeys).toEqual(goTags);
  });
});
