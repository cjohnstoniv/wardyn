-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The AI Run Composer is cut (Stage 1 of the composer refactor): compose_results
-- (0026) was the in-sandbox claude compose wire's proposal-upload handoff row,
-- and every reader/writer of it (PutComposeResult/TakeComposeResult/
-- DiscardComposeResult, the PUT /internal/compose-results/{runID} route) is
-- gone with it. attach_tickets, created in the SAME migration (0026) for the
-- unrelated WS attach-ticket flow, is untouched -- it survives this cut.
--
-- Forward-only, like every migration here: there is no data worth preserving
-- (rows are consume-once with a generous TTL sweep, never a durable record).
DROP TABLE IF EXISTS compose_results;
