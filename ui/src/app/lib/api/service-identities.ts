/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The signed-in person's half of #1508: claim an invitation, and mint, list and revoke the tokens of a
// service identity they sponsor. A claim or a mint answers the token's plaintext once; this module hands it
// to the caller and keeps nothing.
import type {
  ApiToken,
  ClaimInvitationRequest,
  Invitation,
  MintServiceTokenRequest,
  ServiceIdentity,
} from "../types/service-identities";
import { asJson, asNoContent, unwrapList, wfetch, withLimit } from "./core";

const tokensPath = (name: string) => `/me/service-identities/${encodeURIComponent(name)}/tokens`;

export const serviceIdentities = {
  // GET /api/v1/me/invitations -> the caller's own pending invitations.
  async listMyInvitations(): Promise<Invitation[]> {
    return unwrapList<Invitation>(await asJson<unknown>(await wfetch(withLimit("/me/invitations"), { method: "GET" })));
  },

  // POST /api/v1/me/invitations/{id}/claim -> 201 with the new token, `token` holding the plaintext.
  async claimInvitation(id: string, body: ClaimInvitationRequest): Promise<ApiToken> {
    return asJson<ApiToken>(
      await wfetch(`/me/invitations/${encodeURIComponent(id)}/claim`, { method: "POST", body: JSON.stringify(body) }),
    );
  },

  // GET /api/v1/me/service-identities -> the identities the caller sponsors, retired ones included.
  async listMine(): Promise<ServiceIdentity[]> {
    return unwrapList<ServiceIdentity>(
      await asJson<unknown>(await wfetch(withLimit("/me/service-identities"), { method: "GET" })),
    );
  },

  // POST /api/v1/me/service-identities/{name}/tokens -> 201 with the new token, `token` holding the plaintext.
  async mintToken(name: string, body: MintServiceTokenRequest): Promise<ApiToken> {
    return asJson<ApiToken>(await wfetch(tokensPath(name), { method: "POST", body: JSON.stringify(body) }));
  },

  // GET /api/v1/me/service-identities/{name}/tokens -> the identity's tokens, revoked and expired included.
  async listTokens(name: string): Promise<ApiToken[]> {
    return unwrapList<ApiToken>(await asJson<unknown>(await wfetch(withLimit(tokensPath(name)), { method: "GET" })));
  },

  // DELETE /api/v1/me/service-identities/{name}/tokens/{id} -> 204; any other answer throws.
  async revokeToken(name: string, id: string): Promise<void> {
    await asNoContent(await wfetch(`${tokensPath(name)}/${encodeURIComponent(id)}`, { method: "DELETE" }));
  },
};
