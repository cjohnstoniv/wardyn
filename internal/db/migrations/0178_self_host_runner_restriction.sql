-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Self-hosting a runner is off until an admin turns it on (0.9.1). Minting a registration token for a
-- runner of one's own (POST /me/runners/tokens) needs the `feature` capability value `self_host_runner`.
-- This row restricts that value: only an allow grant that names it admits anyone, so a `*` allow grants
-- nobody (capBatch.decide's step 3). An `all` allow for the value is the one-row way to keep the 0.9.0
-- behaviour. Runners and claims that already exist are untouched: only the mint is asked.
--
-- ON CONFLICT keeps an admin's own choice: a database that already holds the row is left as it is.
INSERT INTO capability_restrictions (capability, value, created_by)
VALUES ('feature', 'self_host_runner', 'migration:0178')
ON CONFLICT (capability, value) DO NOTHING;
