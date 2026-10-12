/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Invitations, service identities and API-token rows (#1508): each interface has exactly the json tags of
// the Go struct it mirrors (service-identities.wire.test.ts reads the Go source and diffs the two).

/** Where an invitation stands; derived from the row and the clock, never stored. */
export type InvitationState = "pending" | "claimed" | "expired" | "revoked";

/** internal/types Invitation: GET /me/invitations lists the caller's own pending ones. It holds no secret. */
export interface Invitation {
  id: string;
  /** The one person who can claim it: a principal, not an email. */
  target: string;
  created_by: string;
  created_at: string;
  expires_at: string;
  token_name?: string;
  /** The longest the claimed token lives, in seconds; absent when the deployment's rule decides. */
  token_ttl_seconds?: number;
  /** The id of the admin-minted token the claim revokes. */
  replaces_token_id?: string;
  claimed_at?: string;
  token_id?: string;
  revoked_at?: string;
  revoked_by?: string;
  state: InvitationState;
  /** The create response only. */
  console_path?: string;
}

/** Where a service identity stands. `sponsor_inactive` is derived from the sponsor's session at read. */
export type ServiceIdentityState = "active" | "suspended" | "retired" | "sponsor_inactive";

/** internal/types ServiceIdentity: GET /me/service-identities lists the ones the caller sponsors. */
export interface ServiceIdentity {
  name: string;
  description?: string;
  sponsor: string;
  sponsor_since: string;
  user_type: string;
  created_by: string;
  created_at: string;
  suspended_at?: string;
  suspended_by?: string;
  retired_at?: string;
  retired_by?: string;
  state: ServiceIdentityState;
}

/** Where an admin-minted token stands; the wire values of internal/types TokenDisposition. */
export type TokenDisposition =
  | "needs_decision"
  | "awaiting_owner"
  | "replaced_by_owner"
  | "linked_to_service_identity"
  | "moved_to_service_identity"
  | "revoked";

/**
 * internal/types APIToken. `token` is the plaintext and rides on a create or claim answer only; no list
 * carries it. `disposition` is set on an admin-minted token only.
 */
export interface ApiToken {
  id: string;
  principal: string;
  email?: string;
  role: string;
  user_type: string;
  groups?: string[];
  groups_truncated?: boolean;
  name: string;
  created_at: string;
  last_used_at?: string;
  revoked_at?: string;
  /** Absent when the token never expires. */
  expires_at?: string;
  /** The admin who minted it for its owner; absent when the owner minted it. */
  minted_by?: string;
  disposition?: TokenDisposition;
  /** The service identity an admin linked this token to; set with `disposition` on a token that was moved. */
  service_identity?: string;
  token?: string;
}

/** The POST /me/invitations/{id}/claim body (pkg/client ClaimInvitationRequest). */
export interface ClaimInvitationRequest {
  name: string;
  ttl_seconds?: number;
}

/** The POST /me/service-identities/{name}/tokens body (pkg/client MintServiceTokenRequest). */
export interface MintServiceTokenRequest {
  name: string;
  ttl_seconds?: number;
}
