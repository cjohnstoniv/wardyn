-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Sponsor attach is off until an admin turns it on (0.9). A service identity's sponsor may read and
-- stop the service's runs; entering one (a terminal or a UI app) needs the `feature` capability value
-- `service_attach`. This row restricts that value: only an allow grant that names it admits anyone,
-- so a `*` allow grants nobody (capBatch.decide's step 3). Lifting the restriction is a deliberate
-- PUT /permissions/availability/feature/service_attach.
--
-- ON CONFLICT keeps an admin's own choice: a database that already holds the row is left as it is.
INSERT INTO capability_restrictions (capability, value, created_by)
VALUES ('feature', 'service_attach', 'migration:0167')
ON CONFLICT (capability, value) DO NOTHING;
