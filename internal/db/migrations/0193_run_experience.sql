-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The canonical run mode a person chose at launch (0.9 New Run): a background
-- task or an interactive environment. Written ONCE at create from the request's
-- `experience`, and never re-derived: every interactive door of a run (attach,
-- interactive exec, SSH, the UI gateway, a desktop session, a restored session)
-- reads this stored value, so a background run is refused there whatever the
-- caller sends later.
--
-- experience '' is a run from an older client or from before 0.9: it keeps the
-- behaviour every door had, because the legacy `interactive` flag is an
-- execution detail and not a mode. A run launched by a new client always carries
-- one of the two values.
--
-- Split-role installs grant the app role no extra privilege here: this is a
-- column on a table it already writes.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS experience TEXT NOT NULL DEFAULT '' CHECK (experience IN ('', 'background', 'interactive'));
