-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- User types (0.8, design user-types-design.md §2.1): the org's own names for
-- the kinds of people who use Wardyn ("Portfolio manager", "Contractor"),
-- replacing the one fixed non-admin role. A type carries no controls of its
-- own: governance assignments, capability grants and drive grants name it as a
-- subject (a later migration widens their subject_type CHECKs), so this table
-- is only the list of names.
--
-- id is the immutable slug a role-map value, a grant subject and a token stamp
-- refer to. Lowercase ASCII words joined by single hyphens, so it can never be
-- one of the tier words (security_admin carries an underscore; admin, user,
-- member and denied are refused at the API write boundary, which is where the
-- reserved list lives).
--
-- name is UNIQUE because it is what people see: two types both called
-- "Contractor" could not be told apart on the People page.
--
-- priority breaks a sign-in tie between two CUSTOM types; the built-in type is
-- the tie floor and never takes part, so its priority is always 0.
--
-- built_in marks the seeded 'standard' row: editable (name, description), never
-- deletable. It is what every deployment that never creates a type resolves
-- everyone to, and what the legacy role value "member" becomes.
CREATE TABLE IF NOT EXISTS user_types (
    id          TEXT PRIMARY KEY CHECK (id ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND length(id) <= 63),
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    priority    INT NOT NULL DEFAULT 0,
    built_in    BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by  TEXT NOT NULL DEFAULT '',
    CHECK (NOT built_in OR priority = 0)
);

INSERT INTO user_types (id, name, built_in)
VALUES ('standard', 'Standard user', true)
ON CONFLICT (id) DO NOTHING;
