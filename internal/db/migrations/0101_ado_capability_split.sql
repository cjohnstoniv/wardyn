-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The Azure DevOps per-area capability split. The one "read" capability becomes
-- a read per Azure DevOps area, and three write capabilities split along Azure
-- DevOps' own permission lines. This migration rewrites every stored list to an
-- EXACTLY equivalent one — the same requests permitted, the same Entra scopes
-- requested, so nobody consents again — and there is no alias: after this
-- release the server refuses the old ids.
--
--   read            -> code_read, work_read, wiki_read, build_read, release_read,
--                      serviceendpoint_read, library_read, packaging_read,
--                      test_read, project_read, identity_read
--   work_write      -> work_write, work_admin
--   build_execute   -> build_execute, release_execute
--   build_admin     -> build_admin, release_admin
--   anything else   -> itself
--
-- What it does, in order:
--   1. Every Azure DevOps provider row's entra block: capability_ceiling is
--      split, and default_profile is split — or, when absent or empty (which
--      read as "read" before), written out as the eleven reads.
--   2. run_policies.spec, governance_profiles.ceiling and
--      launch_presets.request.inline_policy: azure_devops_capabilities is split
--      where it is a list.
--   3. Every PENDING Azure DevOps capability escalation is CANCELLED: it names a
--      capability the new catalogue may not grant. One approval.cancel row per
--      run. A pending consent request names scopes, which do not change, and
--      stays pending.
--
-- Not rewritten: credential_grants snapshots (the record of what was granted),
-- decided approvals (what the approver saw), audit_events, and the sealed
-- sidecar configs, which SQL cannot read. A run live during the upgrade is
-- refused at its next resolve (capability_ceiling drift) and must be relaunched.
--
-- A second run over migrated data changes nothing: every new id maps to itself.
-- A value that is not a list is left as it is; the next write refuses it.

CREATE OR REPLACE FUNCTION pg_temp.ado_caps_split(caps jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE AS $fn$
    SELECT CASE WHEN COALESCE(jsonb_typeof(caps), '') <> 'array' THEN caps ELSE COALESCE((
        SELECT jsonb_agg(to_jsonb(c) ORDER BY first_seen)
        FROM (
            SELECT c, min(o * 100 + n) AS first_seen
            FROM jsonb_array_elements_text(caps) WITH ORDINALITY AS e(old, o)
            CROSS JOIN LATERAL unnest(CASE old
                WHEN 'read' THEN ARRAY['code_read', 'work_read', 'wiki_read', 'build_read', 'release_read',
                                       'serviceendpoint_read', 'library_read', 'packaging_read', 'test_read',
                                       'project_read', 'identity_read']
                WHEN 'work_write' THEN ARRAY['work_write', 'work_admin']
                WHEN 'build_execute' THEN ARRAY['build_execute', 'release_execute']
                WHEN 'build_admin' THEN ARRAY['build_admin', 'release_admin']
                ELSE ARRAY[old]
            END) WITH ORDINALITY AS m(c, n)
            GROUP BY c
        ) AS split
    ), '[]'::jsonb) END
$fn$;

DO $$
DECLARE
    cfg jsonb;
BEGIN
    -- 1. Provider rows.
    SELECT config INTO cfg FROM site_config WHERE singleton;
    IF cfg IS NOT NULL AND jsonb_typeof(cfg #> '{workspace_providers,git}') = 'array' THEN
        cfg := jsonb_set(cfg, '{workspace_providers,git}', COALESCE((
            SELECT jsonb_agg(CASE WHEN COALESCE(jsonb_typeof(row -> 'entra'), '') <> 'object' THEN row
                ELSE jsonb_set(row, '{entra}', (row -> 'entra')
                    || CASE WHEN row -> 'entra' ? 'capability_ceiling'
                            THEN jsonb_build_object('capability_ceiling', pg_temp.ado_caps_split(row -> 'entra' -> 'capability_ceiling'))
                            ELSE '{}'::jsonb END
                    || jsonb_build_object('default_profile', pg_temp.ado_caps_split(
                            CASE WHEN COALESCE(row -> 'entra' -> 'default_profile', '[]'::jsonb) IN ('[]'::jsonb, 'null'::jsonb)
                                 THEN '["read"]'::jsonb ELSE row -> 'entra' -> 'default_profile' END)))
                END ORDER BY o)
            FROM jsonb_array_elements(cfg #> '{workspace_providers,git}') WITH ORDINALITY AS t(row, o)), '[]'::jsonb));
        UPDATE site_config SET config = cfg WHERE singleton;
    END IF;

    -- 2. Stored policy lists.
    UPDATE run_policies
    SET spec = jsonb_set(spec, '{azure_devops_capabilities}', pg_temp.ado_caps_split(spec -> 'azure_devops_capabilities'))
    WHERE jsonb_typeof(spec -> 'azure_devops_capabilities') = 'array';
    UPDATE governance_profiles
    SET ceiling = jsonb_set(ceiling, '{azure_devops_capabilities}', pg_temp.ado_caps_split(ceiling -> 'azure_devops_capabilities'))
    WHERE jsonb_typeof(ceiling -> 'azure_devops_capabilities') = 'array';
    UPDATE launch_presets
    SET request = jsonb_set(request, '{inline_policy,azure_devops_capabilities}',
                            pg_temp.ado_caps_split(request #> '{inline_policy,azure_devops_capabilities}'))
    WHERE jsonb_typeof(request #> '{inline_policy,azure_devops_capabilities}') = 'array';

    -- 3. Escalations naming a capability of the old catalogue.
    WITH cancelled AS (
        UPDATE approvals
        SET state = 'CANCELLED', decided_at = now(), decided_by = 'system', reason = 'ado_capability_split'
        WHERE state = 'PENDING' AND requested_scope ->> 'lane' = 'azure_devops' AND requested_scope ? 'capability'
        RETURNING run_id
    ), per_run AS (
        SELECT run_id, count(*) AS cnt FROM cancelled GROUP BY run_id
    )
    INSERT INTO audit_events (id, run_id, actor_type, actor, action, target, outcome, data)
    SELECT gen_random_uuid(), run_id, 'system', 'wardyn/migration', 'approval.cancel', run_id::text, 'success',
           jsonb_build_object('run_id', run_id, 'reason', 'ado_capability_split', 'count', cnt,
                              'by_kind', jsonb_build_object('tool_call:azure_devops', cnt))
    FROM per_run;
END
$$;
