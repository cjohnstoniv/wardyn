-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A governance profile's contact: who owns the profile and how a person it
-- refuses can ask for a change. A closed JSON object (owner, email, request_url,
-- request_text) whose field set and rules live in internal/policyref, as `limits`
-- does for GovernanceLimits. NULL is "no contact"; the CHECK is a backstop for a
-- direct write, and every reader re-validates the fields anyway.
ALTER TABLE governance_profiles
    ADD COLUMN IF NOT EXISTS contact JSONB NULL
        CHECK (contact IS NULL OR (jsonb_typeof(contact) = 'object' AND octet_length(contact::text) <= 8192));
