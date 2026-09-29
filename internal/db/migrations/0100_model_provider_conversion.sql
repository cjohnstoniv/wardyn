-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The model-provider clean break (multi-provider design §2.11). Before this
-- migration a run's model access was decided by three overlapping stores: the
-- AI-kind integrations in site_config.config.integrations, the agent roster's
-- mechanism / credential_source / sso_* fields, and a workspace's
-- llm_cred.integration_ref. From here on the model-provider block
-- (site_config.config.model_providers) is the only one, and this migration
-- converts the other three into it in one transaction. There is no alias
-- window: nothing reads the old fields after this release.
--
-- What it does, in order:
--   1. AI-kind integrations (anthropic_api_key, anthropic_subscription,
--      openai_api_key, bedrock; the pre-base-component {category, type} shape
--      included) become providers of the same id — lowercased, with every
--      character outside [a-z0-9._-] turned into "-" and cut to 64, the provider
--      id grammar (api.modelProviderIDPattern) — enabled on the harnesses of the
--      agent catalog that can drive their kind and that the roster offers (every
--      one when there is no roster). An id a provider already holds maps onto
--      that provider unchanged. A Bedrock row's auth_lane "auto" (or unset)
--      becomes bedrock_sso when the claude-code roster row names bedrock_sso or
--      an AWS sign-in was ever stored, else bedrock_bearer; "bearer" becomes
--      bedrock_bearer; any other lane is kept as that guess but turned off. Its
--      config.region becomes bedrock.region and config.model the claude-code
--      model. A subscription row on lane resident_host (the operator's host
--      ~/.claude) is an org-wide credential and is not converted.
--   2. Each roster row's lane joins a provider of exactly that kind: the one
--      provider of the kind, or the DefaultFor:agent_runs one of several. With
--      none, a provider "<agent>-<mechanism>" is created; with several and no
--      default, that new provider is created turned off. A bedrock_sso row's
--      sso_start_url / sso_account_id / sso_role_name fill the provider's
--      bedrock sign-in setup. bedrock_env and bedrock_aws_dir (the daemon's own
--      AWS environment and the host ~/.aws mount) are org-wide credentials and
--      are not converted. Every row then keeps only id, disabled and
--      default_provider: an admin's own default stands, else the provider the
--      row's lane joined, else the DefaultFor:agent_runs provider serving it.
--   3. A converted or created Bedrock provider with no region, a harness with
--      no model, or (bedrock_sso) no start URL is turned off. Its region and
--      model came from the boot environment, which a migration cannot read.
--   4. workspaces.llm_cred.integration_ref becomes provider_ref naming the
--      provider converted from it, or NULL when nothing was; a pin already set
--      stands. Each rewritten pin writes workspace.llm_cred.migrated.
--   5. The AI-kind integration rows are deleted from site config.
--   6. Every PENDING credential_reauth hold for a model credential (no `lane`
--      in its scope, so not an Azure DevOps sign-in) that names no provider is
--      CANCELLED: after this release a sign-in resolves only the hold of the
--      provider it names, so nothing could ever resolve these. One
--      approval.cancel row per run, as the terminal cascade writes.
--
-- Every item that is not converted, or whose provider is created turned off,
-- writes model_provider.not_converted naming it and why. No credential moves:
-- whose a stored secret was is not knowable from the operator's namespace.
-- Audit rows go through the audit_events chain trigger like any other insert.

DO $$
DECLARE
    cfg          jsonb;
    has_roster   boolean;
    roster       jsonb := '[]'::jsonb;
    existing     jsonb := '[]'::jsonb;
    providers    jsonb := '[]'::jsonb; -- providers this migration creates, in order
    intmap       jsonb := '{}'::jsonb; -- integration id -> provider id
    defaults     text[] := '{}';       -- providers converted from a DefaultFor:agent_runs row
    audited      text[] := '{}';       -- providers already named by a not_converted row
    sso_captured boolean;
    r            jsonb;
    rc           jsonb;
    k            text;
    lane         text;
    pid          text;
    pkind        text;
    reason       text;
    region       text;
    model        text;
    harn         jsonb;
    agent        text;
    mech         text;
    target       text;
    matches      text[];
    borrow       jsonb;
    i            int;
    n            int;
    new_roster   jsonb := '[]'::jsonb;
    row_default  text;
    ws           record;
    ref_from     text;
    ref_to       text;
    ai_kinds     CONSTANT text[] := ARRAY['anthropic_api_key', 'anthropic_subscription', 'openai_api_key', 'bedrock'];
BEGIN
    SELECT config INTO cfg FROM site_config WHERE singleton;
    IF cfg IS NULL THEN
        cfg := '{}'::jsonb;
    END IF;
    has_roster := COALESCE(jsonb_typeof(cfg -> 'agent_providers') = 'object', false);
    IF has_roster THEN
        roster := COALESCE(cfg -> 'agent_providers' -> 'agents', '[]'::jsonb);
    END IF;
    IF jsonb_typeof(cfg -> 'model_providers' -> 'providers') = 'array' THEN
        existing := cfg -> 'model_providers' -> 'providers';
    END IF;
    sso_captured := EXISTS (SELECT 1 FROM secrets WHERE name = 'wardyn-harness-aws-oauth');

    -- 1. AI-kind integrations.
    FOR r IN SELECT value FROM jsonb_array_elements(COALESCE(cfg -> 'integrations', '[]'::jsonb)) LOOP
        k := COALESCE(r ->> 'kind', NULLIF(r ->> 'type', ''), r ->> 'category');
        CONTINUE WHEN NOT (k = ANY (ai_kinds));
        rc := COALESCE(r -> 'config', '{}'::jsonb);
        IF k = 'anthropic_subscription' AND rc ->> 'lane' = 'resident_host' THEN
            INSERT INTO audit_events (id, actor_type, actor, action, target, outcome, data)
            VALUES (gen_random_uuid(), 'system', 'wardyn/migration', 'model_provider.not_converted', r ->> 'id', 'success',
                    jsonb_build_object('source', 'integration', 'id', r ->> 'id', 'kind', k, 'reason', 'org_wide_credential'));
            CONTINUE;
        END IF;
        pid := left(regexp_replace(lower(COALESCE(r ->> 'id', '')), '[^a-z0-9._-]', '-', 'g'), 64);
        IF pid <> '' AND EXISTS (SELECT 1 FROM jsonb_array_elements(existing) p WHERE p ->> 'id' = pid) THEN
            intmap := intmap || jsonb_build_object(COALESCE(r ->> 'id', ''), pid);
            CONTINUE;
        END IF;
        pid := COALESCE(NULLIF(pid, ''), k);
        n := 1;
        WHILE EXISTS (SELECT 1 FROM jsonb_array_elements(existing || providers) p WHERE p ->> 'id' = pid) LOOP
            n := n + 1;
            pid := left(regexp_replace(lower(COALESCE(NULLIF(r ->> 'id', ''), k)), '[^a-z0-9._-]', '-', 'g'), 60) || '-' || n;
        END LOOP;

        pkind := k;
        reason := NULL;
        region := NULL;
        model := NULL;
        IF k = 'bedrock' THEN
            lane := COALESCE(rc ->> 'auth_lane', rc ->> 'lane', '');
            pkind := CASE WHEN lane = 'bearer' THEN 'bedrock_bearer'
                          WHEN sso_captured OR EXISTS (
                              SELECT 1 FROM jsonb_array_elements(roster) a
                              WHERE a ->> 'id' = 'claude-code' AND a ->> 'mechanism' = 'bedrock_sso') THEN 'bedrock_sso'
                          ELSE 'bedrock_bearer' END;
            IF lane NOT IN ('', 'auto', 'bearer') THEN
                reason := 'unknown_bedrock_auth_lane';
            END IF;
            region := NULLIF(rc ->> 'region', '');
            model := NULLIF(rc ->> 'model', '');
        END IF;
        harn := '[]'::jsonb;
        FOREACH agent IN ARRAY CASE WHEN pkind = 'openai_api_key' THEN ARRAY['codex-cli'] ELSE ARRAY['claude-code'] END LOOP
            IF NOT has_roster OR EXISTS (SELECT 1 FROM jsonb_array_elements(roster) a WHERE a ->> 'id' = agent) THEN
                harn := harn || jsonb_build_array(jsonb_strip_nulls(jsonb_build_object('harness', agent, 'model', model)));
            END IF;
        END LOOP;
        providers := providers || jsonb_build_array(jsonb_strip_nulls(jsonb_build_object(
            'id', pid, 'uid', gen_random_uuid()::text,
            'name', NULLIF(left(btrim(COALESCE(r ->> 'name', '')), 100), ''),
            'kind', pkind,
            'disabled', CASE WHEN COALESCE((r ->> 'disabled')::boolean, false) THEN true END,
            'bedrock', CASE WHEN pkind LIKE 'bedrock\_%' THEN jsonb_strip_nulls(jsonb_build_object('region', region)) END,
            'harnesses', harn)));
        intmap := intmap || jsonb_build_object(COALESCE(r ->> 'id', ''), pid);
        IF COALESCE(r -> 'default_for', '[]'::jsonb) ? 'agent_runs' THEN
            defaults := defaults || pid;
        END IF;
        IF reason IS NOT NULL THEN
            providers := jsonb_set(providers, ARRAY[(jsonb_array_length(providers) - 1)::text, 'disabled'], 'true');
            INSERT INTO audit_events (id, actor_type, actor, action, target, outcome, data)
            VALUES (gen_random_uuid(), 'system', 'wardyn/migration', 'model_provider.not_converted', r ->> 'id', 'success',
                    jsonb_build_object('source', 'integration', 'id', r ->> 'id', 'kind', k, 'reason', reason, 'provider', pid));
            audited := audited || pid;
        END IF;
    END LOOP;

    -- 2. Roster rows.
    FOR r IN SELECT value FROM jsonb_array_elements(roster) LOOP
        agent := r ->> 'id';
        mech := COALESCE(r ->> 'mechanism', '');
        target := NULL;
        reason := NULL;
        IF mech IN ('bedrock_env', 'bedrock_aws_dir') THEN
            INSERT INTO audit_events (id, actor_type, actor, action, target, outcome, data)
            VALUES (gen_random_uuid(), 'system', 'wardyn/migration', 'model_provider.not_converted', agent, 'success',
                    jsonb_build_object('source', 'agent_provider', 'id', agent, 'kind', mech, 'reason', 'org_wide_credential'));
        ELSIF mech IN ('anthropic_subscription', 'anthropic_api_key', 'openai_api_key', 'bedrock_bearer', 'bedrock_sso') THEN
            SELECT array_agg(p ->> 'id' ORDER BY o) INTO matches
            FROM jsonb_array_elements(existing || providers) WITH ORDINALITY AS t(p, o)
            WHERE p ->> 'kind' = mech;
            matches := COALESCE(matches, '{}');
            IF cardinality(matches) = 1 THEN
                target := matches[1];
            ELSIF cardinality(matches) > 1 THEN
                SELECT array_agg(m) INTO matches FROM unnest(matches) m WHERE m = ANY (defaults);
                IF cardinality(COALESCE(matches, '{}')) = 1 THEN
                    target := matches[1];
                ELSE
                    reason := 'several_providers_no_default';
                END IF;
            END IF;
            IF target IS NULL THEN
                -- A new provider for this lane; a Bedrock one borrows the region
                -- and model of the one Bedrock provider converted above, if any.
                pid := left(regexp_replace(lower(agent || '-' || mech), '[^a-z0-9._-]', '-', 'g'), 64);
                n := 1;
                WHILE EXISTS (SELECT 1 FROM jsonb_array_elements(existing || providers) p WHERE p ->> 'id' = pid) LOOP
                    n := n + 1;
                    pid := left(regexp_replace(lower(agent || '-' || mech), '[^a-z0-9._-]', '-', 'g'), 60) || '-' || n;
                END LOOP;
                borrow := NULL;
                IF mech LIKE 'bedrock\_%' THEN
                    SELECT CASE WHEN count(*) = 1 THEN min(p::text)::jsonb END INTO borrow
                    FROM jsonb_array_elements(providers) p WHERE p ->> 'kind' LIKE 'bedrock\_%';
                END IF;
                providers := providers || jsonb_build_array(jsonb_strip_nulls(jsonb_build_object(
                    'id', pid, 'uid', gen_random_uuid()::text, 'kind', mech,
                    'bedrock', CASE WHEN mech LIKE 'bedrock\_%' THEN
                        jsonb_strip_nulls(jsonb_build_object('region', borrow -> 'bedrock' ->> 'region')) END,
                    'harnesses', jsonb_build_array(jsonb_strip_nulls(jsonb_build_object('harness', agent,
                        'model', (SELECT h ->> 'model' FROM jsonb_array_elements(borrow -> 'harnesses') h
                                  WHERE h ->> 'harness' = agent)))))));
                target := pid;
                IF reason = 'several_providers_no_default' THEN
                    providers := jsonb_set(providers, ARRAY[(jsonb_array_length(providers) - 1)::text, 'disabled'], 'true');
                    INSERT INTO audit_events (id, actor_type, actor, action, target, outcome, data)
                    VALUES (gen_random_uuid(), 'system', 'wardyn/migration', 'model_provider.not_converted', agent, 'success',
                            jsonb_build_object('source', 'agent_provider', 'id', agent, 'kind', mech, 'reason', reason, 'provider', pid));
                    audited := audited || pid;
                END IF;
                reason := NULL;
            END IF;
            -- Join the target (a provider this migration created: an admin's own
            -- provider is never edited, and is the default only if it serves).
            SELECT o - 1 INTO i FROM jsonb_array_elements(providers) WITH ORDINALITY AS t(p, o) WHERE p ->> 'id' = target;
            IF i IS NOT NULL THEN
                IF NOT EXISTS (SELECT 1 FROM jsonb_array_elements(providers -> i -> 'harnesses') h WHERE h ->> 'harness' = agent) THEN
                    providers := jsonb_set(providers, ARRAY[i::text, 'harnesses'],
                        COALESCE(providers -> i -> 'harnesses', '[]'::jsonb) || jsonb_build_array(jsonb_build_object('harness', agent)));
                END IF;
                IF mech = 'bedrock_sso' AND NULLIF(r ->> 'sso_start_url', '') IS NOT NULL
                   AND COALESCE(providers -> i -> 'bedrock' ->> 'sso_start_url', '') = '' THEN
                    providers := jsonb_set(providers, ARRAY[i::text, 'bedrock'],
                        COALESCE(providers -> i -> 'bedrock', '{}'::jsonb) || jsonb_strip_nulls(jsonb_build_object(
                            'sso_start_url', r ->> 'sso_start_url',
                            'sso_account_id', NULLIF(r ->> 'sso_account_id', ''),
                            'sso_role_name', NULLIF(r ->> 'sso_role_name', ''))));
                END IF;
            ELSIF NOT EXISTS (SELECT 1 FROM jsonb_array_elements(existing) p, jsonb_array_elements(p -> 'harnesses') h
                              WHERE p ->> 'id' = target AND h ->> 'harness' = agent) THEN
                INSERT INTO audit_events (id, actor_type, actor, action, target, outcome, data)
                VALUES (gen_random_uuid(), 'system', 'wardyn/migration', 'model_provider.not_converted', agent, 'success',
                        jsonb_build_object('source', 'agent_provider', 'id', agent, 'kind', mech,
                                           'reason', 'provider_does_not_serve_agent', 'provider', target));
                target := NULL;
            END IF;
        END IF;
        row_default := COALESCE(NULLIF(r ->> 'default_provider', ''), target);
        new_roster := new_roster || jsonb_build_array(jsonb_strip_nulls(jsonb_build_object(
            'id', agent,
            'disabled', CASE WHEN COALESCE((r ->> 'disabled')::boolean, false) THEN true END,
            'default_provider', row_default)));
    END LOOP;

    -- DefaultFor:agent_runs reaches a roster row that is still without a default.
    FOR i IN 0 .. jsonb_array_length(new_roster) - 1 LOOP
        CONTINUE WHEN new_roster -> i ? 'default_provider';
        SELECT p ->> 'id' INTO target FROM jsonb_array_elements(providers) WITH ORDINALITY AS t(p, o)
        WHERE p ->> 'id' = ANY (defaults)
          AND EXISTS (SELECT 1 FROM jsonb_array_elements(p -> 'harnesses') h WHERE h ->> 'harness' = new_roster -> i ->> 'id')
        ORDER BY o LIMIT 1;
        IF FOUND THEN
            new_roster := jsonb_set(new_roster, ARRAY[i::text, 'default_provider'], to_jsonb(target));
        END IF;
    END LOOP;
    -- With no roster there is no row to carry a default: say so where it
    -- mattered, where more than one provider serves the harness.
    IF NOT has_roster THEN
        FOR r IN SELECT p FROM jsonb_array_elements(providers) p WHERE p ->> 'id' = ANY (defaults) LOOP
            FOR agent IN SELECT h ->> 'harness' FROM jsonb_array_elements(r -> 'harnesses') h LOOP
                CONTINUE WHEN (SELECT count(*) FROM jsonb_array_elements(existing || providers) p, jsonb_array_elements(p -> 'harnesses') h
                               WHERE h ->> 'harness' = agent AND NOT COALESCE((p ->> 'disabled')::boolean, false)) < 2;
                INSERT INTO audit_events (id, actor_type, actor, action, target, outcome, data)
                VALUES (gen_random_uuid(), 'system', 'wardyn/migration', 'model_provider.not_converted', r ->> 'id', 'success',
                        jsonb_build_object('source', 'default_for', 'id', r ->> 'id', 'kind', r ->> 'kind',
                                           'reason', 'default_needs_agent_roster', 'provider', r ->> 'id', 'harness', agent));
            END LOOP;
        END LOOP;
    END IF;

    -- 3. A created Bedrock provider that cannot serve is turned off.
    FOR i IN 0 .. jsonb_array_length(providers) - 1 LOOP
        r := providers -> i;
        CONTINUE WHEN r ->> 'kind' NOT LIKE 'bedrock\_%';
        reason := CASE
            WHEN COALESCE(r -> 'bedrock' ->> 'region', '') = ''
              OR EXISTS (SELECT 1 FROM jsonb_array_elements(r -> 'harnesses') h WHERE COALESCE(h ->> 'model', '') = '')
                THEN 'region_or_model_from_boot_env'
            WHEN r ->> 'kind' = 'bedrock_sso' AND COALESCE(r -> 'bedrock' ->> 'sso_start_url', '') = ''
                THEN 'no_sso_start_url'
        END;
        CONTINUE WHEN reason IS NULL;
        providers := jsonb_set(providers, ARRAY[i::text, 'disabled'], 'true');
        CONTINUE WHEN r ->> 'id' = ANY (audited);
        INSERT INTO audit_events (id, actor_type, actor, action, target, outcome, data)
        VALUES (gen_random_uuid(), 'system', 'wardyn/migration', 'model_provider.not_converted', r ->> 'id', 'success',
                jsonb_build_object('source', 'model_provider', 'id', r ->> 'id', 'kind', r ->> 'kind', 'reason', reason,
                                   'provider', r ->> 'id'));
    END LOOP;

    -- 4. Workspace pins.
    FOR ws IN SELECT id, llm_cred FROM workspaces WHERE llm_cred IS NOT NULL LOOP
        ref_from := NULLIF(ws.llm_cred ->> 'integration_ref', '');
        ref_to := COALESCE(NULLIF(ws.llm_cred ->> 'provider_ref', ''), intmap ->> ref_from);
        UPDATE workspaces
        SET llm_cred = CASE WHEN ref_to IS NULL THEN NULL ELSE jsonb_build_object('provider_ref', ref_to) END
        WHERE id = ws.id;
        IF ref_from IS NOT NULL THEN
            INSERT INTO audit_events (id, actor_type, actor, action, target, outcome, data)
            VALUES (gen_random_uuid(), 'system', 'wardyn/migration', 'workspace.llm_cred.migrated', ws.id::text, 'success',
                    jsonb_build_object('workspace', ws.id, 'integration_ref', ref_from, 'provider_ref', ref_to));
        END IF;
    END LOOP;

    -- 5. Write the converted document; the AI-kind integration rows go.
    cfg := (cfg - 'integrations') || CASE
        WHEN EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(cfg -> 'integrations', '[]'::jsonb)) x
                     WHERE NOT (COALESCE(x ->> 'kind', NULLIF(x ->> 'type', ''), x ->> 'category') = ANY (ai_kinds)))
        THEN jsonb_build_object('integrations', (
            SELECT jsonb_agg(x ORDER BY o) FROM jsonb_array_elements(cfg -> 'integrations') WITH ORDINALITY AS t(x, o)
            WHERE NOT (COALESCE(x ->> 'kind', NULLIF(x ->> 'type', ''), x ->> 'category') = ANY (ai_kinds))))
        ELSE '{}'::jsonb END;
    IF has_roster THEN
        cfg := jsonb_set(cfg, '{agent_providers}', jsonb_build_object('agents', new_roster));
    END IF;
    IF jsonb_array_length(existing || providers) > 0 THEN
        cfg := jsonb_set(cfg, '{model_providers}', jsonb_build_object('providers', existing || providers));
    END IF;
    UPDATE site_config SET config = cfg WHERE singleton;

    -- 6. Holds nothing can resolve any more.
    WITH cancelled AS (
        UPDATE approvals
        SET state = 'CANCELLED', decided_at = now(), decided_by = 'system', reason = 'model_provider_conversion'
        WHERE state = 'PENDING' AND kind = 'credential_reauth'
          AND NOT (requested_scope ? 'lane') AND NOT (requested_scope ? 'provider')
        RETURNING run_id,
                  CASE WHEN COALESCE(requested_scope ->> 'credential_source', '') <> ''
                       THEN 'credential_reauth:aws_sso' ELSE 'credential_reauth' END AS tally
    ), per_kind AS (
        SELECT run_id, tally, count(*) AS cnt FROM cancelled GROUP BY run_id, tally
    )
    INSERT INTO audit_events (id, run_id, actor_type, actor, action, target, outcome, data)
    SELECT gen_random_uuid(), run_id, 'system', 'wardyn/migration', 'approval.cancel', run_id::text, 'success',
           jsonb_build_object('run_id', run_id, 'reason', 'model_provider_conversion',
                              'count', sum(cnt), 'by_kind', jsonb_object_agg(tally, cnt))
    FROM per_kind GROUP BY run_id;
END
$$;
