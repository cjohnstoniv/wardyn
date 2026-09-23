-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Tightened limits reach live runs (long-holds design rev 4, §2.2, RL-8). When
-- a profile's run limits tighten, every live run that captured that profile is
-- re-clamped on the next sweep; end_tightened_at records when that re-clamp
-- moved the run's end, so the run page can say "Your admin shortened the limit.
-- This run now ends …". A person moving the end afterwards clears it. NULL is
-- an end nobody re-clamped, which is every legacy row. Metadata-only on PG11+.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS end_tightened_at TIMESTAMPTZ;
