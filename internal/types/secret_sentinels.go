// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// The sentinel secret names: grant-carried names that resolve to a live
// credential at inject time rather than a stored secret.

// SubscriptionOAuthSecret is a SENTINEL secret name (NOT a stored secret): an
// api_key grant carrying it resolves at inject time to the operator's LIVE
// Anthropic subscription OAuth token (resident ~/.claude), never a stored
// value — so subscription runs are credentialed proxy-side like api-key runs
// while the sandbox holds only the inert sentinel. Also the durable "this
// profile uses subscription LLM auth" marker on a recorded profile, since the
// ~/.claude mount that would otherwise signal it is never synthesized. Shared
// here so api + recordmode + UI agree on the name.
const SubscriptionOAuthSecret = "anthropic-subscription-oauth"

// ManagedOAuthSecret is a SENTINEL secret name (NOT a stored secret) that
// works exactly like SubscriptionOAuthSecret at the injection sink
// (host-pinned to api.anthropic.com, forced Authorization: Bearer, value
// masked), but resolves to the Wardyn-MANAGED subscription token: a
// long-lived `claude setup-token` OAuth token the operator captured via the
// container-login flow and Wardyn persisted (internal/api/harnesscred.go).
// This is what lets a distroless COMPOSE/containerized deployment, with no
// host ~/.claude to read, credential a subscription run proxy-side without
// ever making the token resident in the sandbox. Distinct from
// SubscriptionOAuthSecret only in SOURCE (managed store vs resident host
// ~/.claude), so the audit trail names which one credentialed a run.
const ManagedOAuthSecret = "anthropic-managed-oauth"

// AWSSSOAccessTokenSecret is a SENTINEL secret name (NOT a stored secret) that
// works like the two OAuth sentinels above at the injection sink (host-pinned,
// forced header, masked), resolving to the run's OWN captured AWS IAM
// Identity Center access token — the credential an `aws sso login` captured
// into wardynd's secret store as a whole blob, not as a value under this name.
//
// This is what makes portal.sso.<region> GetRoleCredentials (`authtype:none`)
// work: the proxy carries the session as the x-amz-sso_bearer_token HEADER
// and the sandbox never holds the token. The resolver, internal/api's
// resolveAWSSSOInjection, also re-derives the credential's scope from the
// roster and refuses any drift from the snapshot the grant was authored with.
//
// Deliberately NOT in sinkReservedSecret: that guard refuses names an api_key
// grant must never resolve, but this name's entire purpose is to be resolved
// through that sink, host-pinned, exactly as bedrock-api-key is
// (internal/api/secrets.go explains why that one is excluded too). Nothing is
// stored under it — a secrets-API Put of this name is a value the sentinel
// arm never reads.
const AWSSSOAccessTokenSecret = "aws-sso-access-token"

// ADOEntraAccessTokenSecret is a SENTINEL secret name (NOT a stored secret)
// that resolves to an Azure DevOps access token minted from the RUN OWNER's
// own captured Entra sign-in (internal/api's resolveADOInjection). Nothing is
// stored under this name: the credential in the store is the person's refresh
// token, held in the reserved `wardyn-harness-ado-<row>-oauth` blob, and the
// access token exists only for the moment it is handed to the run's proxy
// sidecar.
//
// SECURITY: THE TOKEN DOES NOT BOUND THE RUN, and no reader of this name may
// assume it does — measured against a real tenant, Entra issues an Azure
// DevOps token carrying EVERY scope the person consented to regardless of
// what subset is requested, so what holds a run to its granted capabilities
// is Wardyn's own capability check in front of the resource (the proxy), not
// the credential. The granted scope string the authority reports is recorded
// on the audit row because a token is opaque and that string is the only
// honest evidence of what the credential can do.
//
// Deliberately NOT in sinkReservedSecret, for AWSSSOAccessTokenSecret's
// reason: being resolved at that sink, host-pinned, is the whole point.
const ADOEntraAccessTokenSecret = "azure-devops-entra-access-token"
