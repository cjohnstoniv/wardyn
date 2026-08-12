/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Self-service SSH gateway key registry — the signed-in human's own keys,
// scoped server-side by principal. No admin view of anyone else's.
import type { SSHPublicKey } from "../types";
import { asJson, errText, HttpError, wfetch } from "./core";

export const sshKeys = {
  // GET /api/v1/me/ssh-keys
  async listKeys(): Promise<SSHPublicKey[]> {
    const res = await wfetch("/me/ssh-keys", { method: "GET" });
    return asJson<SSHPublicKey[]>(res);
  },

  // POST /api/v1/me/ssh-keys {name?, public_key} -> the stored row (server
  // computes the fingerprint from the parsed key; a private key or anything
  // unparseable is refused, 422).
  async addKey(name: string, publicKey: string): Promise<SSHPublicKey> {
    const res = await wfetch("/me/ssh-keys", {
      method: "POST",
      body: JSON.stringify({ name, public_key: publicKey }),
    });
    return asJson<SSHPublicKey>(res);
  },

  // DELETE /api/v1/me/ssh-keys/{fingerprint} -> 204.
  // fingerprint is ssh.FingerprintSHA256's raw-base64 "SHA256:…" form, which
  // routinely contains '/' — MUST be percent-encoded as one path segment, or
  // the value chi's router hands the handler is still escaped and never
  // matches the stored (decoded) fingerprint. encodeURIComponent, same as
  // secrets.ts's setSecret/deleteSecret(name).
  async deleteKey(fingerprint: string): Promise<void> {
    const res = await wfetch(`/me/ssh-keys/${encodeURIComponent(fingerprint)}`, { method: "DELETE" });
    // 204 has no body — do not asJson/res.json() it (matches
    // secrets.ts's deleteSecret; a 404 is also tolerated as "already gone").
    if (!res.ok && res.status !== 404) {
      throw new HttpError(res.status, await errText(res));
    }
  },
};
