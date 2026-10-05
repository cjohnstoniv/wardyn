/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The People page's two small pure mappings: a wire role to its label, and a row to its state.
import { PEOPLE } from "../../../lib/people-access-copy";
import type { PersonSummary } from "../../../lib/types";
import { PEOPLE_PAGE as P } from "../../wardyn/copy/people";

// The same labels the access panel uses (people-access-copy.ts), so a role reads one way everywhere.
// An unrecognized wire value renders itself rather than a role it is not.
export function roleLabel(role: string): string {
  switch (role) {
    case "admin":
      return PEOPLE.ROLE_ADMIN;
    case "security_admin":
      return PEOPLE.ROLE_SECURITY_ADMIN;
    case "user":
    case "member":
      return PEOPLE.ROLE_USER;
    case "unknown":
      return PEOPLE.ROLE_UNKNOWN;
    default:
      return role;
  }
}

export type PersonState = "active" | "deactivated" | "never";

// Deactivated wins: a person SCIM deactivated stays Deactivated whether or not they ever signed in.
export function personState(p: PersonSummary): PersonState {
  if (p.deactivated_at) return "deactivated";
  return p.last_signed_in_at ? "active" : "never";
}

export const STATE_LABEL: Record<PersonState, string> = {
  active: P.STATE_ACTIVE,
  deactivated: P.STATE_DEACTIVATED,
  never: P.STATE_NEVER,
};
