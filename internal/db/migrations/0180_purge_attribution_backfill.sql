-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A purge now also clears the person as the revoker of a runner, the minter of a registration token made
-- for someone else, and the linker of a token move. Those clears run inside the purge's runners and
-- service-identities steps, which a person purged earlier already has marked done, so nothing would run
-- them for that person. Reopening the two steps lets the purge sweeper re-enter each such purge once; the
-- steps are idempotent updates keyed by the person, and the purge's audit row is not rewritten.
UPDATE deprovision_jobs
   SET state = 'pending', updated_at = now()
 WHERE kind = 'purge' AND step IN ('runners', 'service_identities') AND state = 'done';
