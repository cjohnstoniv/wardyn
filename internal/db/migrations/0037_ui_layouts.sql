-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Per-user run-detail cockpit widget layout: the arrangement a human gives
-- the run cockpit's evidence widgets (timeline, terminal, identity, egress,
-- grants, ...) is THEIRS and has to survive a new machine, so it is a server
-- row keyed by principal — ui/src/app/lib/storage.ts is localStorage and is
-- explicitly NOT the answer here (see internal/api/ui_layout.go's doc
-- comment).
--
-- preset splits the row in two per principal: "live" (a run still executing)
-- and "finished" (one that has stopped) genuinely want different
-- arrangements — a live run's cockpit foregrounds the terminal/timeline, a
-- finished one foregrounds audit/recording — and a human rearranging one
-- must not have the other silently reshuffled underneath them. The set is
-- CLOSED and validated server-side (internal/api/ui_layout.go's
-- runLayoutPresets); this table places no CHECK on it, matching how this
-- schema validates other closed Go-side enums (ApprovalKind, GrantSpec.Kind)
-- in application code rather than SQL.
--
-- layout is the widget list [{widget,x,y,w,h}], already validated against the
-- known-widget-id closed set and bounded in count by the API before this
-- table is ever written to (ui_layout.go again) — this table trusts its
-- caller the same way ssh_public_keys trusts the gateway's own parse step.
--
-- PRIMARY KEY (principal, preset) is both the natural key and exactly what
-- PutRunLayout's ON CONFLICT upsert needs: one row per principal per preset,
-- always, no separate existence check before writing.
--
-- NUMBERING: authored as 0037 — by the time this lane reached the migrations
-- directory, 0035 and 0036 had already been claimed by other concurrently-
-- landing lanes (the same parallel-lane collision 0033's header names).
CREATE TABLE IF NOT EXISTS ui_run_layouts (
    principal  TEXT NOT NULL,
    preset     TEXT NOT NULL,
    layout     JSONB NOT NULL DEFAULT '[]'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (principal, preset)
);
