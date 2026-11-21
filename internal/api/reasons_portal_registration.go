// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// portal registration reasons
const (
	reasonPortalRegistrationInvalid        = "portal_registration_invalid"                // a registration with no client id, or an access-token registration with no audience or scopes
	reasonPortalIssuersNotActivated        = "portal_issuers_not_activated"               // issuer portals have not been switched on for this deployment
	reasonPortalIssuersNeedSSO             = "portal_issuers_need_sso"                    // issuer portals need console sign-in configured: the console issuer is what they must not equal
	reasonDelegateIssuerPinRequired        = "delegate_issuer_pin_required"               // an issuer portal names no unit to run in
	reasonDelegateIssuerURLInvalid         = "delegate_issuer_url_invalid"                // the issuer or internal address is not an https URL with a host and no userinfo, query or fragment
	reasonDelegateInternalIssuerNotListed  = "delegate_internal_issuer_not_allowed"       // the internal address names a host the deployment does not list
	reasonDelegateIssuerIsConsole          = "delegate_issuer_is_console"                 // the issuer is, after normalising, one this deployment has served as its console issuer
	reasonDelegateIssuerSpellingConflict   = "delegate_issuer_spelling_conflict"          // another spelling of a bound issuer is already registered
	reasonDelegateIssuerAddressConflict    = "delegate_issuer_address_conflict"           // the bound issuer fetches from another internal address
	reasonDelegateIssuerProfileConflict    = "delegate_issuer_profile_conflict"           // the bound issuer names a person by another claim
	reasonDelegateNamespaceInUse           = "delegate_namespace_in_use"                  // stored records already name people under this issuer's key
	reasonDelegateTokenProfileInvalid      = "delegate_token_profile_invalid"             // token_profile is not okta_access_token or okta_id_token
	reasonDelegateAccessFieldsNeedAccess   = "delegate_access_fields_need_access_profile" // audience or required_scopes on an ID-token portal
	reasonDelegateEmailDomainsNeedIDToken  = "delegate_email_domains_need_id_token"       // email_domains on an access-token portal
	reasonDelegateEmailDomainInvalid       = "delegate_email_domain_invalid"              // an email domain is empty, has an @ or is not a host name
	reasonDelegateGroupsClaimInvalid       = "delegate_groups_claim_invalid"              // groups_claim is not a claim name
	reasonDelegateUserTypeInvalid          = "delegate_user_type_invalid"                 // user_type is missing or names no user type
	reasonDelegateMaxAgeInvalid            = "delegate_max_age_invalid"                   // max_subject_token_age_seconds outside 300-600
	reasonDelegateGrantTTLInvalid          = "delegate_grant_ttl_invalid"                 // grant_ttl_seconds below 300
	reasonDelegateGrantTTLAboveMaxAge      = "delegate_grant_ttl_above_max_age"           // grant_ttl_seconds above the portal's maximum token age
	reasonDelegateGroupOrPortalManaged     = "delegate_group_or_portal_managed"           // an issuer portal with no group must say portal_managed_admission
	reasonDelegatePortalManagedWithGroup   = "delegate_portal_managed_with_group"         // portal_managed_admission with a group
	reasonDelegatePortalManagedNeedsIssuer = "delegate_portal_managed_needs_issuer"       // portal_managed_admission on a console-issuer portal
	reasonDelegateClaimIncomplete          = "delegate_claim_incomplete"                  // required_claim names only one of claim and value
	reasonDelegateClaimReserved            = "delegate_claim_reserved"                    // required_claim names a claim that already has a meaning
	reasonDelegateClaimInvalid             = "delegate_claim_invalid"                     // required_claim's name or value breaks its shape
	reasonDelegateRenewalNeedsIssuer       = "delegate_renewal_needs_issuer"              // ui_session_renewal on a console-issuer portal
	reasonDelegateIssuerFieldNeedsIssuer   = "delegate_issuer_field_needs_issuer"         // a field only an issuer portal has, on a console-issuer registration
)
