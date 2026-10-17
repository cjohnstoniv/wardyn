-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A purge now also clears the person as the revoker of a runner, the minter of a registration token made
-- for someone else, and the linker of a token move. Those clears run inside the purge's runners and
-- service-identities steps, which a person purged earlier already has marked done, so nothing would run
-- them for that person. Reopening the two steps lets the purge sweeper re-enter each such purge once; the
-- steps are idempotent updates keyed by the person, and the purge's audit row is not rewritten.
--
-- A person purged before 0.9.0 has no rows for the runners, invitations and service-identities steps at
-- all, because those steps did not exist: nothing would re-enter that purge either. Each such person,
-- whose purge finished (its audit step is done), gets the three steps as pending rows, which the sweeper
-- picks up the same way.
INSERT INTO deprovision_jobs (identity_id, kind, step, target)
SELECT i.id, 'purge', s.step, ''
  FROM principal_identities i
 CROSS JOIN (VALUES ('runners'), ('invitations'), ('service_identities')) AS s (step)
 WHERE i.purged_at IS NOT NULL
   AND EXISTS (SELECT 1 FROM deprovision_jobs a
                WHERE a.identity_id = i.id AND a.kind = 'purge' AND a.step = 'audit_deprovision' AND a.state = 'done')
ON CONFLICT (identity_id, kind, step, target) DO NOTHING;

UPDATE deprovision_jobs
   SET state = 'pending', updated_at = now()
 WHERE kind = 'purge' AND step IN ('runners', 'service_identities') AND state = 'done';
