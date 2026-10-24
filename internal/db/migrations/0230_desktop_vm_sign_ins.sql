-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Sign-ins in a Linux VM's desktop (#2972): one row per VM run whose guest reported its sign-in
-- store at boot. store 'memory' is a desktop whose keychain and browser profiles are tmpfs (the
-- agent's row in the Agents tab allowed sign-ins); 'none' is one where they are read-only, so it
-- keeps no sign-ins. A run with no row is one whose guest or runner predates the store: the console
-- then says nothing about sign-ins. paused_at and paused_by are set while the person has paused the
-- agent to sign in; never a secret.
--
-- Split-role installs grant the app role SELECT, INSERT, UPDATE, DELETE on it (CHANGELOG, Before
-- you upgrade).
CREATE TABLE IF NOT EXISTS desktop_vm_sign_ins (
    run_id    uuid PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    store     text NOT NULL CHECK (store IN ('memory', 'none')),
    paused_by text,
    paused_at timestamptz
);
