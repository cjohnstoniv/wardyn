/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// User types (0.8, UT-1) — mirrors internal/types.UserType exactly. The
// User types screen (screens/user-types) is the sole editor; the People step,
// the subject pickers (Permissions / Governance / Drives) and the run header
// all read this same shape off GET /user-types or GET /access.

// The built-in type's id — never deletable, and the sign-in tie floor.
// Mirrors internal/types.UserTypeStandard.
export const USER_TYPE_STANDARD = "standard";

export interface UserType {
  id: string;
  name: string;
  description: string;
  // Breaks a sign-in tie between two custom types (higher wins). Always 0 on
  // the built-in type.
  priority: number;
  built_in: boolean;
  created_at: string;
  updated_at: string;
  created_by?: string;
}

// POST/PUT /user-types body. id is optional on create (derived from name) and
// on update must equal the path's — an id is what role-map values and grant
// subjects point at, so it never changes.
export interface UserTypeInput {
  id?: string;
  name: string;
  description: string;
  priority: number;
}
