-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Why a registered runner could not connect, as the runner last said: the stage its last failed attempt
-- (to connect, to start, or to reach the run relay) stopped at, and when. last_failure_stage is a word from
-- a closed Go list (runnerwire.Stage) checked where the runner's caps event is read, and is '' until the
-- runner reports one; it is never an error's text. last_failure_ongoing is true while the run relay is
-- lost. All three are runner_asserted labels: nothing is admitted or placed by them.
--
-- ADDITIVE ONLY: three new runners columns whose defaults leave every existing row with no failure. A
-- downgrade stops reading them.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS last_failure_stage TEXT NOT NULL DEFAULT '';
ALTER TABLE runners ADD COLUMN IF NOT EXISTS last_failure_at TIMESTAMPTZ;
ALTER TABLE runners ADD COLUMN IF NOT EXISTS last_failure_ongoing BOOLEAN NOT NULL DEFAULT false;
