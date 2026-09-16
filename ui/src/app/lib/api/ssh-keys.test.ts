/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, afterEach } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { sshKeys } from "./ssh-keys";
import { HttpError } from "./core";

// X2-F2 — ssh-keys.tsx's own component test (ssh-keys.test.tsx) mocks this
// whole module, so the module's ACTUAL wire shape (the request this app
// really sends) had zero coverage. Reached here through fetch, stubbed at the
// global — the same technique access.test.ts/recordings.test.ts use for their
// api modules.

// Review fix (F6): the "server's field name" claim below used to be a
// hardcoded TS guess with nothing tying it to the Go struct it names — the
// same repoRoot()/goJSONTags() idiom runs.wire.fields.test.ts uses for its
// own source-parity check, scoped to the one two-field struct this module
// talks to.
function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  const tags: string[] = [];
  for (const t of m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)) {
    if (t[1] !== "-") tags.push(t[1]);
  }
  return tags;
}

describe("sshKeys — wire shape", () => {
  afterEach(() => vi.unstubAllGlobals());

  const jsonResponse = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

  it("listKeys GETs /api/v1/me/ssh-keys and returns the array verbatim", async () => {
    const rows = [
      { fingerprint: "SHA256:abc", principal: "alice@example.com", name: "laptop", public_key: "", created_at: "2026-01-01T00:00:00Z" },
    ];
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(rows));
    vi.stubGlobal("fetch", fetchMock);

    const keys = await sshKeys.listKeys();
    expect(keys).toEqual(rows);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/me/ssh-keys");
    expect(init.method).toBe("GET");
  });

  it("addKey POSTs the server's own field names (internal/api/sshkeys.go's addSSHKeyRequest)", async () => {
    const root = repoRoot();
    const sshkeysGo = readFileSync(join(root, "internal/api/sshkeys.go"), "utf8");
    const goTags = goJSONTags(sshkeysGo, "addSSHKeyRequest");

    const stored = { fingerprint: "SHA256:new", principal: "alice@example.com", name: "phone", public_key: "", created_at: "2026-01-02T00:00:00Z" };
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(stored, 201));
    vi.stubGlobal("fetch", fetchMock);

    const res = await sshKeys.addKey("phone", "ssh-ed25519 AAAA…");
    expect(res).toEqual(stored);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/me/ssh-keys");
    expect(init.method).toBe("POST");
    const sentKeys = Object.keys(JSON.parse(init.body as string));
    expect(sentKeys.sort()).toEqual([...goTags].sort());
    expect(JSON.parse(init.body as string)).toEqual({ name: "phone", public_key: "ssh-ed25519 AAAA…" });
  });

  // The fingerprint is ssh.FingerprintSHA256's raw-base64 "SHA256:…" form,
  // which routinely contains '/' — it MUST be percent-encoded as ONE path
  // segment or chi's router never matches the stored (decoded) fingerprint.
  it("deleteKey percent-encodes a fingerprint containing '/' into one path segment", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);

    await sshKeys.deleteKey("SHA256:ab/cd+ef==");
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/me/ssh-keys/SHA256%3Aab%2Fcd%2Bef%3D%3D");
    expect(init.method).toBe("DELETE");
  });

  // deleteKey tolerates a 404 (matches secrets.ts's deleteSecret: "already
  // gone" is not a failure the caller needs to see).
  it("deleteKey does not throw on a 404", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 404 })));
    await expect(sshKeys.deleteKey("SHA256:gone")).resolves.toBeUndefined();
  });

  // A non-404 failure surfaces as a real HttpError, not a swallowed/generic
  // one — deleteKey only special-cases 404 ("already gone").
  it("deleteKey surfaces a non-404 failure as an HttpError with the server's message", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: "database unavailable" }), { status: 500, headers: { "Content-Type": "application/json" } })),
    );
    const err = await sshKeys.deleteKey("SHA256:boom").catch((e) => e);
    expect(err).toBeInstanceOf(HttpError);
    expect((err as HttpError).status).toBe(500);
  });
});
