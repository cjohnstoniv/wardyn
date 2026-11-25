-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Attach tickets name the one door they open (#3279): purpose '' is a terminal or UI ticket as before; ssh-exec,
-- ssh-forward and ssh-editor open GET /runs/{id}/ssh; attach-forward opens GET /runs/{id}/attach/forward. ports
-- binds a forwarding purpose to the ports it was minted for. Additive: an older replica neither writes nor reads them.
ALTER TABLE attach_tickets ADD COLUMN IF NOT EXISTS purpose TEXT NOT NULL DEFAULT '';
ALTER TABLE attach_tickets ADD COLUMN IF NOT EXISTS ports INTEGER[];
