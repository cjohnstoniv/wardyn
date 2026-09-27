// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// The sentinel secret names: names a grant carries that resolve to a live
// credential at inject time rather than to a stored secret.

// SubscriptionOAuthSecret is a SENTINEL secret name (NOT a stored secret). An
// api_key injection grant carrying it resolves at inject time to the operator's
// LIVE Anthropic subscription OAuth token (from the resident ~/.claude), not a
// value in the secret store — so subscription runs are credentialed proxy-side
// like api-key runs, the sandbox holding only the inert sentinel. It also serves
// as the durable "this profile uses subscription LLM auth" marker on a recorded
// profile (the resident ~/.claude mount that would otherwise signal subscription
// is never synthesized). Shared here so api + recordmode + UI agree on the name.
const SubscriptionOAuthSecret = "anthropic-subscription-oauth"

// ManagedOAuthSecret is a SENTINEL secret name (NOT a stored secret) that works
// exactly like SubscriptionOAuthSecret at the injection sink (host-pinned to
// api.anthropic.com, forced Authorization: Bearer, value masked), but resolves
// to the Wardyn-MANAGED subscription token: a long-lived `claude setup-token`
// OAuth token the operator captured via the container-login flow and Wardyn
// persisted (see internal/api/harnesscred.go). This is what lets a COMPOSE/
// containerized deployment — whose distroless wardynd has no host ~/.claude to
// read — credential a subscription run proxy-side without ever making the token
// resident in the sandbox. Distinct from SubscriptionOAuthSecret only in its
// SOURCE (managed store vs resident host ~/.claude), so the audit trail names
// which one credentialed a run.
const ManagedOAuthSecret = "anthropic-managed-oauth"

// AWSSSOAccessTokenSecret is a SENTINEL secret name (NOT a stored secret) that
// works exactly like the two OAuth sentinels above at the injection sink
// (host-pinned, forced header, masked), and resolves to the run's OWN captured
// AWS IAM Identity Center access token -- the credential an `aws sso login`
// captured into wardynd's secret store as a whole blob, not as a value under
// this name.
//
// It is what makes Phase B (0.7.6) possible: portal.sso.<region>
// GetRoleCredentials is `authtype:none`, so the proxy can carry the session as
// the x-amz-sso_bearer_token HEADER and the sandbox never holds the token. The
// resolver is internal/api's resolveAWSSSOInjection, which additionally
// re-derives the credential's scope from the roster and refuses any drift from
// the snapshot the grant was authored with.
//
// Deliberately NOT in sinkReservedSecret. That guard refuses names an api_key
// grant must never resolve; this name's ENTIRE purpose is to be resolved
// through that sink, host-pinned, exactly as bedrock-api-key is (see
// internal/api/secrets.go, which explains why that one is excluded too). And
// nothing is stored under it: a secrets-API Put of this name would be a value
// the sentinel arm never reads.
const AWSSSOAccessTokenSecret = "aws-sso-access-token"

// ADOEntraAccessTokenSecret is a SENTINEL secret name (NOT a stored secret),
// the fourth of them, and it resolves to an Azure DevOps access token minted
// from the RUN OWNER's own captured Entra sign-in (internal/api's
// resolveADOInjection). Nothing is stored under this name: the credential in
// the store is the person's refresh token, held in the reserved
// `wardyn-harness-ado-<row>-oauth` blob, and the access token exists only for
// the moment it is handed to the run's proxy sidecar.
//
// THE TOKEN DOES NOT BOUND THE RUN, and no reader of this name may assume it
// does. Measured against a real tenant, Entra issues an Azure DevOps token
// carrying EVERY scope the person consented to whatever subset is requested,
// so what holds a run to its granted capabilities is Wardyn's own capability
// check in front of the resource — the proxy — not the credential. The
// granted scope string the authority reports is recorded on the audit row
// because a token is opaque and that string is the only honest evidence of
// what the credential can do.
//
// Deliberately NOT in sinkReservedSecret, for AWSSSOAccessTokenSecret's
// reason: being resolved at that sink, host-pinned, is the whole point.
const ADOEntraAccessTokenSecret = "azure-devops-entra-access-token"
