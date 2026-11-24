-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A run's outage terms (#4082), fixed at dispatch and written once.
CREATE TABLE run_outage_terms (
    run_id         uuid PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    allowance_s    integer NOT NULL CHECK (allowance_s BETWEEN 1800 AND 86400),
    strict         boolean NOT NULL,
    end_unattended boolean NOT NULL,
    written_at     timestamptz NOT NULL DEFAULT now()
);
