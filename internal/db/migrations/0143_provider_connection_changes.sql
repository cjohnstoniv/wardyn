-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

CREATE TABLE IF NOT EXISTS provider_connection_changes (
    owner text NOT NULL CHECK (owner <> ''),
    provider_id text NOT NULL,
    provider_uid text NOT NULL,
    reason text NOT NULL CHECK (reason IN ('destination_changed', 'kind_changed')),
    changed_at timestamptz NOT NULL,
    new_destination text NOT NULL,
    PRIMARY KEY (owner, provider_uid)
);

-- Every capture and replacement clears history in the credential's own transaction.
CREATE OR REPLACE FUNCTION clear_provider_connection_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM provider_connection_changes
    WHERE owner = NEW.owned_by
      AND (NEW.name IN (
          'wardyn-provider-' || provider_uid || '-key',
          'wardyn-provider-' || provider_uid || '-oauth',
          'wardyn-provider-' || provider_uid || '-sso',
          'wardyn-provider-' || provider_uid || '-entra'
      ) OR EXISTS (
          -- A failed kind-change save leaves a prospective UID until this owner reconnects.
          SELECT 1 FROM site_config,
              jsonb_array_elements(COALESCE(NULLIF(config->'model_providers'->'providers', 'null'::jsonb), '[]'::jsonb)) AS p(provider)
          WHERE p.provider->>'id' = provider_connection_changes.provider_id
            AND NEW.name IN (
                'wardyn-provider-' || (p.provider->>'uid') || '-key',
                'wardyn-provider-' || (p.provider->>'uid') || '-oauth',
                'wardyn-provider-' || (p.provider->>'uid') || '-sso',
                'wardyn-provider-' || (p.provider->>'uid') || '-entra'
            )
      ));
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS secrets_clear_provider_connection_change ON secrets;
CREATE TRIGGER secrets_clear_provider_connection_change
AFTER INSERT OR UPDATE OF ciphertext, kek_id ON secrets
FOR EACH ROW EXECUTE FUNCTION clear_provider_connection_change();
