-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The harnesses each MCP component's server was turned on for at launch. Revive
-- re-derives the proxy's harness bindings from it. Harness ids are configuration,
-- not personal data, so an erasure keeps them as it keeps ordinal and
-- self_defined. Old rows carry no MCP server and need no backfill; the app role
-- already writes this table.
ALTER TABLE run_components ADD COLUMN IF NOT EXISTS mcp_harnesses TEXT[] NOT NULL DEFAULT '{}';
