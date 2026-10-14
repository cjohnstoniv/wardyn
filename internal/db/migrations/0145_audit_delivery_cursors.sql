-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The acknowledged SIEM delivery checkpoint (#1513): one row per destination holding the
-- acknowledged seq, the row_hash that seq carried, when it was accepted, and the halt state.
-- The leader-only delivery loop (internal/audit/sinks) reads the masked trail from it and
-- advances it only on the collector's acceptance; retention past it is a reported reset, never a blocked drop.
CREATE TABLE IF NOT EXISTS audit_delivery_cursors (
    destination    text PRIMARY KEY CHECK (destination <> ''),
    acked_seq      bigint NOT NULL DEFAULT 0,
    acked_row_hash text   NOT NULL DEFAULT '',
    acked_at       timestamptz,
    last_error     text   NOT NULL DEFAULT '',
    halted         boolean NOT NULL DEFAULT false,
    resets         bigint NOT NULL DEFAULT 0,
    updated_at     timestamptz NOT NULL DEFAULT now()
);
