-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0
--
-- Units become a tree (the policy tree, 0.10.0). One Default unit is added: its walls are the
-- deployment default (WARDYN_DEFAULT_POLICY), read at resolve, so it stores none. A composed unit
-- that built on the deployment default becomes Default's child, which composes exactly as before.
-- A standalone unit stays top-level. Every unit gets a kind, a mode (own_walls, or pass_through:
-- its parent's walls exactly) and its depth from the top.
--
-- A person may now hold several memberships, one per unit: the unique key moves from
-- (subject_type, subject) to (unit_id, subject_type, subject). Every membership that existed
-- before this migration is marked legacy; per person only the precedence winner among legacy
-- rows is a context, which is today's one assignment, so no run's walls change.
--
-- No table is new, so a split-role install needs no grant change.
--
-- Re-runnable: every step is guarded, so re-applying it over a migrated schema changes nothing.

ALTER TABLE governance_units
    ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'unit',
    ADD COLUMN IF NOT EXISTS mode text NOT NULL DEFAULT 'own_walls',
    ADD COLUMN IF NOT EXISTS depth smallint NOT NULL DEFAULT 1;

-- The Default unit (store.DefaultUnitID). It takes the first label no unit holds, in the order
-- Default, Default (deployment), Default (deployment 2), ...: the name is UNIQUE, so an install that
-- already has units named Default and Default (deployment) must not collide.
INSERT INTO governance_units (id, name, kind, ceiling, limits)
SELECT '00000000-0000-0000-0000-000000000001',
       (SELECT c.label
          FROM (SELECT 0 AS n, 'Default' AS label
                UNION ALL SELECT 1, 'Default (deployment)'
                UNION ALL SELECT g, 'Default (deployment ' || g || ')' FROM generate_series(2, 1000) AS g) c
         WHERE NOT EXISTS (SELECT 1 FROM governance_units u WHERE u.name = c.label)
         ORDER BY c.n
         LIMIT 1),
       'default', '{}', '{}'
WHERE NOT EXISTS (SELECT 1 FROM governance_units WHERE id = '00000000-0000-0000-0000-000000000001')
ON CONFLICT (id) DO NOTHING;

UPDATE governance_units SET parent_id = '00000000-0000-0000-0000-000000000001'
WHERE overlay IS NOT NULL AND parent_id IS NULL AND kind = 'unit';

-- Depth from the top: Default and every parent-less unit are depth 1.
WITH RECURSIVE tree AS (
    SELECT id, 1 AS d FROM governance_units WHERE parent_id IS NULL
    UNION ALL
    SELECT u.id, t.d + 1 FROM governance_units u JOIN tree t ON u.parent_id = t.id WHERE t.d < 64
)
UPDATE governance_units u SET depth = tree.d FROM tree WHERE u.id = tree.id AND u.depth <> tree.d;

CREATE UNIQUE INDEX IF NOT EXISTS governance_units_one_default ON governance_units ((true)) WHERE kind = 'default';

ALTER TABLE governance_units
    DROP CONSTRAINT IF EXISTS governance_units_kind_check,
    ADD CONSTRAINT governance_units_kind_check CHECK (kind IN ('unit', 'default')),
    DROP CONSTRAINT IF EXISTS governance_units_mode_check,
    ADD CONSTRAINT governance_units_mode_check CHECK (mode IN ('own_walls', 'pass_through')),
    DROP CONSTRAINT IF EXISTS governance_units_depth_check,
    ADD CONSTRAINT governance_units_depth_check CHECK (depth BETWEEN 1 AND 64),
    DROP CONSTRAINT IF EXISTS governance_units_default_shape,
    ADD CONSTRAINT governance_units_default_shape CHECK (kind <> 'default' OR (parent_id IS NULL AND overlay IS NULL)),
    DROP CONSTRAINT IF EXISTS governance_units_unit_shape,
    ADD CONSTRAINT governance_units_unit_shape CHECK (kind <> 'unit' OR ((parent_id IS NULL) = (overlay IS NULL))),
    DROP CONSTRAINT IF EXISTS governance_units_pass_through_shape,
    ADD CONSTRAINT governance_units_pass_through_shape CHECK (mode <> 'pass_through' OR overlay = '{}'::jsonb);

ALTER TABLE governance_memberships DROP CONSTRAINT IF EXISTS governance_memberships_subject_type_subject_key;
ALTER TABLE governance_memberships DROP CONSTRAINT IF EXISTS governance_memberships_unit_subject_key;
ALTER TABLE governance_memberships ADD CONSTRAINT governance_memberships_unit_subject_key UNIQUE (unit_id, subject_type, subject);

ALTER TABLE governance_memberships ADD COLUMN IF NOT EXISTS home boolean NOT NULL DEFAULT false;
-- Added with DEFAULT true, so every row that exists now is legacy without an UPDATE; rows written
-- after the upgrade default to false. A re-run finds the column and changes nothing.
ALTER TABLE governance_memberships ADD COLUMN IF NOT EXISTS legacy boolean NOT NULL DEFAULT true;
ALTER TABLE governance_memberships ALTER COLUMN legacy SET DEFAULT false;
