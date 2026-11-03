-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Every capture and replacement clears history in the credential's own transaction. 0143 listed the key, oauth,
-- sso and entra parts; an endpoint sign-in's oidc part clears it too.
CREATE OR REPLACE FUNCTION clear_provider_connection_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM provider_connection_changes
    WHERE owner = NEW.owned_by
      AND (NEW.name IN (
          'wardyn-provider-' || provider_uid || '-key',
          'wardyn-provider-' || provider_uid || '-oauth',
          'wardyn-provider-' || provider_uid || '-sso',
          'wardyn-provider-' || provider_uid || '-entra',
          'wardyn-provider-' || provider_uid || '-oidc'
      ) OR EXISTS (
          -- A failed kind-change save leaves a prospective UID until this owner reconnects.
          SELECT 1 FROM site_config,
              jsonb_array_elements(COALESCE(NULLIF(config->'model_providers'->'providers', 'null'::jsonb), '[]'::jsonb)) AS p(provider)
          WHERE p.provider->>'id' = provider_connection_changes.provider_id
            AND NEW.name IN (
                'wardyn-provider-' || (p.provider->>'uid') || '-key',
                'wardyn-provider-' || (p.provider->>'uid') || '-oauth',
                'wardyn-provider-' || (p.provider->>'uid') || '-sso',
                'wardyn-provider-' || (p.provider->>'uid') || '-entra',
                'wardyn-provider-' || (p.provider->>'uid') || '-oidc'
            )
      ));
    RETURN NEW;
END;
$$;
