-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- What became of a macOS VM run's disk on its Mac (#3652): one row per macOS VM run whose runner reported
-- its teardown. state 'removed' is a run directory the runner deleted, the VM's disk and clone with it;
-- 'kept' is one still on the Mac, with reason 'helper_alive' (the VM's helper outlived its kill),
-- 'remove_failed' (the delete failed) or '' (none the console knows). A later sweep that deletes it turns
-- 'kept' into 'removed'; nothing turns 'removed' back. A run with no row is one whose runner predates the
-- report or has not sent it yet: the console then says the removal is under way for a short while after
-- the run's end, and nothing after that. at is when the runner says it happened. Never a secret.
--
-- Split-role installs grant the app role SELECT, INSERT, UPDATE, DELETE on it (CHANGELOG, Before
-- you upgrade).
CREATE TABLE IF NOT EXISTS macos_vm_disks (
    run_id uuid PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    state  text NOT NULL CHECK (state IN ('removed', 'kept')),
    reason text NOT NULL DEFAULT '' CHECK (reason IN ('', 'helper_alive', 'remove_failed')),
    at     timestamptz NOT NULL,
    CHECK (state = 'kept' OR reason = '')
);

-- A "finished" layout saved before this release has no "Attach from your terminal" tile (widget ssh): a
-- finished run could not show one, so the catalog never offered it there. A finished macOS VM run arranged
-- by such a layout would never show its disk line. Each saved finished layout with widgets and no ssh tile
-- gains one below everything else, where the console's own add puts it (x 0, width 4, height 5). It runs
-- once, so a person who then removes the tile keeps it removed. Re-runnable: a layout with the tile, an
-- empty one ("never saved") and every live one are left as they are. updated_at is not touched.
-- A saved y or h is any non-negative Go int (the layout PUT bounds neither from above), so the bottom is
-- summed as numeric, never int, and the new tile's y is capped where y + h still fits a 32-bit integer: a
-- layout already that tall gets the tile at the cap instead of failing the migration and the boot with it.
UPDATE ui_run_layouts
   SET layout = layout || jsonb_build_array(jsonb_build_object('widget', 'ssh', 'x', 0, 'w', 4, 'h', 5,
       'y', (SELECT LEAST(COALESCE(ceil(max((e->>'y')::numeric + (e->>'h')::numeric)), 0), 2147483642)::bigint
               FROM jsonb_array_elements(layout) AS e
              WHERE jsonb_typeof(e->'y') = 'number' AND jsonb_typeof(e->'h') = 'number')))
 WHERE preset = 'finished' AND jsonb_typeof(layout) = 'array' AND layout <> '[]'::jsonb
   AND NOT layout @> '[{"widget": "ssh"}]'::jsonb;
