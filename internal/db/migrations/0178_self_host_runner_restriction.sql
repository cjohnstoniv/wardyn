-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Self-hosting a runner is off until an admin turns it on (0.9.1). Minting a registration token for a
-- runner of one's own (POST /me/runners/tokens) needs the `feature` capability value `self_host_runner`.
-- This row restricts that value: only an allow grant that names it admits anyone, so a `*` allow grants
-- nobody (capBatch.decide's step 3). An `all` allow for the value is the one-row way to keep the 0.9.0
-- behaviour. Runners and claims that already exist are untouched: only the mint is asked.
--
-- 0.9.0 cannot stage that grant before the upgrade (its write boundary rejects the unknown value), so an
-- estate that already self-hosts would be refused until an admin acted. The second statement therefore
-- adds the `all` allow when a claimed runner exists, whoever minted its registration token (the person
-- themself or an admin for them), and an estate with none starts closed. Narrow or delete the grant
-- afterwards; the restriction stays. A database that applied this file before it was widened (a runner
-- an admin minted did not count then) is not re-asked: setup status names a restricted value with
-- claimed runners and no grant row, and gives the grant to write.
--
-- ON CONFLICT keeps an admin's own choice: a database that already holds either row is left as it is.
INSERT INTO capability_restrictions (capability, value, created_by)
VALUES ('feature', 'self_host_runner', 'migration:0178')
ON CONFLICT (capability, value) DO NOTHING;

INSERT INTO capability_grants (id, subject_type, subject, capability, value, effect, created_by)
SELECT gen_random_uuid(), 'all', '', 'feature', 'self_host_runner', 'allow', 'migration:0178'
WHERE EXISTS (SELECT 1 FROM runners WHERE state = 'claimed')
ON CONFLICT (subject_type, subject, capability, value) DO NOTHING;
