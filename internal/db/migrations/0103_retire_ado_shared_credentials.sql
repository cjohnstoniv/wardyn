-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Azure DevOps stops using shared credentials (#1429). A shared PAT or SSH key
-- is one account's standing credential, used for every person's runs; from
-- this release an Azure DevOps row's credentials are per person. This
-- migration rewrites every stored Azure DevOps provider row that still names a
-- shared lane, and turns it OFF. It deletes nothing: the row keeps its
-- addresses and its id, and an admin turns it back on once they have chosen how
-- their people connect. Until then the row's hosts are withdrawn from
-- effective_scm_hosts and from run egress, a launch on them is refused with a
-- reason that names the row, and GET /site-config lists them under
-- withheld_scm_hosts with the row that withholds each.
--
-- A row is "shared" when its lanes name pat or ssh, or are empty (an empty list
-- read as the legacy pat, ssh and app lanes). For each such row, in order:
--   1. A row that also carries the entra lane keeps it and is otherwise left
--      as it is (still enabled): only pat and ssh leave its lanes.
--   2. A row with no per-person lane left, on Azure DevOps Services only
--      (dev.azure.com, *.visualstudio.com) becomes disabled, lanes ["entra"],
--      per_user, with an entra block of token_mode own_pat, the ceiling
--      project_read + code_read, and the default profile empty — a person pastes
--      their own token. own_pat names no tenant or client: nothing signs in.
--   3. A row on any other host (Azure DevOps Server, which has no Entra
--      sign-in) becomes disabled, lanes ["pat"], per_user, with no entra block.
--
-- Not rewritten: GitHub rows, and every stored secret. SQL cannot delete a
-- store-mode secret's value, so the stored shared credentials
-- (git-pat-<host>, ssh-key-<host> and its known-hosts-<host>) are deleted by
-- wardynd itself, ONCE, at the first start after this migration, per
-- namespace, and audited as ado_shared_credential.retire. boot_once holds the
-- marker that makes it once: the sweep runs only while the row's done_at is
-- null and sets it when it has finished, so a token a person stores under
-- the same name afterwards is never swept.
--
-- A second run over migrated data changes nothing: a row that already carries
-- only a per_user pat lane, or the entra lane, is left alone.

CREATE TABLE IF NOT EXISTS boot_once (
    name    text PRIMARY KEY,
    done_at timestamptz
);
INSERT INTO boot_once (name) VALUES ('ado_shared_credential_retire') ON CONFLICT DO NOTHING;

CREATE OR REPLACE FUNCTION pg_temp.ado_retire_shared(r jsonb) RETURNS jsonb
LANGUAGE plpgsql IMMUTABLE AS $fn$
DECLARE
    lanes jsonb := CASE WHEN jsonb_typeof(r -> 'lanes') = 'array' THEN r -> 'lanes' ELSE '[]'::jsonb END;
    hosted boolean;
BEGIN
    IF r ->> 'kind' IS DISTINCT FROM 'azure_devops' THEN
        RETURN r;
    END IF;
    -- No shared lane named and lanes not empty: nothing to retire.
    IF jsonb_array_length(lanes) > 0 AND NOT (lanes ? 'pat' OR lanes ? 'ssh') THEN
        RETURN r;
    END IF;
    -- Already the per-person Server shape this migration writes.
    IF lanes = '["pat"]'::jsonb AND r ->> 'credential_source' = 'per_user' THEN
        RETURN r;
    END IF;
    -- A per-person lane survives: only the shared ones leave.
    IF lanes ? 'entra' THEN
        RETURN jsonb_set(r, '{lanes}', COALESCE((
            SELECT jsonb_agg(l ORDER BY o)
            FROM jsonb_array_elements(lanes) WITH ORDINALITY AS t(l, o)
            WHERE l NOT IN ('"pat"'::jsonb, '"ssh"'::jsonb)), '[]'::jsonb));
    END IF;
    hosted := CASE WHEN jsonb_typeof(r -> 'base_urls') = 'array' THEN COALESCE((
        SELECT bool_and(h = 'dev.azure.com' OR h LIKE '%.visualstudio.com')
        FROM (SELECT lower(substring(u FROM '^[A-Za-z][A-Za-z0-9+.-]*://([^/:?#@]+)')) AS h
              FROM jsonb_array_elements_text(r -> 'base_urls') AS u) AS hosts), false)
        ELSE false END;
    IF hosted THEN
        RETURN (r - 'entra') || jsonb_build_object(
            'disabled', true,
            'lanes', '["entra"]'::jsonb,
            'credential_source', 'per_user',
            'entra', jsonb_build_object(
                'token_mode', 'own_pat',
                'capability_ceiling', '["project_read", "code_read"]'::jsonb,
                'default_profile', '[]'::jsonb));
    END IF;
    RETURN (r - 'entra') || jsonb_build_object(
        'disabled', true,
        'lanes', '["pat"]'::jsonb,
        'credential_source', 'per_user');
END
$fn$;

DO $$
DECLARE
    cfg jsonb;
BEGIN
    SELECT config INTO cfg FROM site_config WHERE singleton;
    IF cfg IS NOT NULL AND jsonb_typeof(cfg #> '{workspace_providers,git}') = 'array' THEN
        cfg := jsonb_set(cfg, '{workspace_providers,git}', COALESCE((
            SELECT jsonb_agg(pg_temp.ado_retire_shared(row) ORDER BY o)
            FROM jsonb_array_elements(cfg #> '{workspace_providers,git}') WITH ORDINALITY AS t(row, o)), '[]'::jsonb));
        UPDATE site_config SET config = cfg WHERE singleton;
    END IF;
END
$$;
