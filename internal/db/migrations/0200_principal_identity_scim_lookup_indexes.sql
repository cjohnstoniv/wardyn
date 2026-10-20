-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A SCIM provisioning sync looks a user up once per user by userName, externalId or email
-- (store.PG.SearchIdentities, store.PG.IdentitiesByAlias), and nothing indexed those columns, so each
-- lookup read every identity under the issuer. Each lookup arm now has an index:
--   lower(scim_user_name), lower(scim_external_id) and object_id under the issuer;
--   email_lower, leading, so an erasure naming an email (store.PG.PrincipalsNamed), which has no issuer,
--     reads it as well as a SCIM lookup does;
--   an alias's value_lower alone, because the alias lookups name no kind and the (kind, value_lower)
--     index of 0113 cannot serve a lookup that does not.
CREATE INDEX IF NOT EXISTS principal_identities_scim_user_name_idx
    ON principal_identities (issuer, lower(scim_user_name));
CREATE INDEX IF NOT EXISTS principal_identities_scim_external_id_idx
    ON principal_identities (issuer, lower(scim_external_id));
CREATE INDEX IF NOT EXISTS principal_identities_object_id_idx
    ON principal_identities (issuer, object_id);
CREATE INDEX IF NOT EXISTS principal_identities_email_lower_idx
    ON principal_identities (email_lower, issuer);
CREATE INDEX IF NOT EXISTS principal_identity_aliases_value_lower_idx
    ON principal_identity_aliases (value_lower);
