// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// entra_capture.go is the part of a per-person Entra sign-in that differs by
// kind. The capture itself (the authorization-code leg, the identity binding,
// the sealed refresh-token blob, the single-flight redemption) is one
// implementation; what a kind owns is its SCOPE POLICY and the names it stores
// under.
//
//   - Azure DevOps asks for and keeps exact scope literals (`vso.code`, ...)
//     and refuses `.default` everywhere. That policy is the one this package
//     has always had and is unchanged.
//   - azure_foundry asks for ONE resource, as `<resource>/.default`, and keeps
//     whatever delegated permissions Entra expands it to. Entra answers a
//     `.default` request with the permissions it expands to (for the Foundry
//     audience, `<resource>/user_impersonation`), never with the `.default`
//     string, so an exact-literal intersect would store nothing and every
//     redemption would fail its consent check. The ceiling is therefore a
//     RESOURCE: a capture is usable when the grant holds at least one scope
//     under it.
//   - azure_arm is an Azure CLI row's capture, for Azure Resource Manager: the
//     same one-resource policy as azure_foundry, under the one audience below,
//     stored under the row's own name in the CLI sign-in namespace.

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// entraKind names which scope policy a capture runs under.
type entraKind string

const (
	entraKindADO          entraKind = "azure_devops"
	entraKindAzureFoundry entraKind = "azure_foundry"
	entraKindAzureARM     entraKind = "azure_arm"
	// entraKindEndpointOIDC is a custom_endpoint's own sign-in at its authority (endpoint_oidc.go).
	entraKindEndpointOIDC entraKind = "endpoint_oidc"
)

// azureARMAudience is the one audience an Azure CLI row's sign-in is captured
// for: Azure Resource Manager. It is not in azureAudienceClosed, so a model
// provider's grant can never be pointed at it, and an Azure CLI capture names
// no other.
const azureARMAudience = "https://management.azure.com/.default"

// The two audiences an azure_foundry sign-in may be captured for, as the
// `<resource>/.default` literal Entra is asked for. The Foundry route's
// audience and the Cognitive Services audience are the ones Microsoft Learn
// names for the two routes. Closed on purpose: a redemption refuses anything
// else before it builds a token request, so a tampered snapshot cannot point a
// person's refresh token at a resource this table does not name.
const (
	azureFoundryAudience           = "https://ai.azure.com/.default"
	azureCognitiveServicesAudience = "https://cognitiveservices.azure.com/.default"
)

// azureDefaultSuffix is what turns a resource into its `.default` scope.
const azureDefaultSuffix = "/.default"

// azureAudienceForRoute is the per-route audience table: the one audience a row
// serving route is captured for. ok=false for a route the table does not name.
func azureAudienceForRoute(route string) (string, bool) {
	switch route {
	case types.AzureRouteAnthropic:
		return azureFoundryAudience, true
	case types.AzureRouteOpenAIV1:
		return azureCognitiveServicesAudience, true
	}
	return "", false
}

// azureAudienceClosed reports whether audience is one of the two literals above.
func azureAudienceClosed(audience string) bool {
	return audience == azureFoundryAudience || audience == azureCognitiveServicesAudience
}

// azureAudienceResource is the resource an audience literal names, without the
// trailing `/.default`.
func azureAudienceResource(audience string) string {
	return strings.TrimSuffix(audience, azureDefaultSuffix)
}

// entraCapture is one capture's identity: which kind of scope policy governs
// it, the scopes it asks for, and where its blob lives.
type entraCapture struct {
	kind entraKind
	// scopes is the ceiling. Azure DevOps: the row's scope literals.
	// azure_foundry: exactly the one audience literal.
	scopes []string
	// secretName is the sealed store name the blob is written under.
	secretName string
	// donePath is the console destination a successful capture lands on.
	donePath string
	// rowUID keys the capture's row: the provider's server-minted uid for
	// azure_foundry, the row's admin id for Azure DevOps.
	rowUID string
	// authority is the token endpoint and client of a kind that is not an Entra application (endpoint_oidc.go);
	// nil for every Entra kind, which derive theirs from the application's configuration.
	authority *tokenAuthority
	// oidc is what an endpoint sign-in binds an identity token against; nil for every Entra kind.
	oidc *endpointOIDCBinding
	// redeemFor is the row an endpoint redemption resolved authority from (endpointOIDCStillBound); nil
	// otherwise.
	redeemFor *types.ModelProvider
}

// adoCapture is the capture an Azure DevOps row runs. Callers have validated
// cfg.RowID (adoEntraSecretName panics on one that is not a usable name).
func adoCapture(cfg ADOEntraConfig) entraCapture {
	return entraCapture{
		kind:       entraKindADO,
		scopes:     cfg.Scopes,
		secretName: adoEntraSecretName(cfg.RowID),
		donePath:   adoSignInDonePath,
		rowUID:     cfg.RowID,
	}
}

// azureFoundryCapture is the capture an azure_foundry row serves: one row uid
// and the audience its route names. It refuses a uid that is not usable as a
// store name and an audience that is not one of the two closed literals.
func azureFoundryCapture(uid, audience string) (entraCapture, error) {
	if !adoEntraValidRowID(uid) {
		return entraCapture{}, fmt.Errorf("azure foundry sign-in: provider uid %q is not usable as a store name", uid)
	}
	if !azureAudienceClosed(audience) {
		return entraCapture{}, fmt.Errorf("azure foundry sign-in: audience %q is not one this deployment captures", audience)
	}
	return entraCapture{
		kind:       entraKindAzureFoundry,
		scopes:     []string{audience},
		secretName: providerSecretName(uid, providerEntraPart),
		donePath:   azureFoundrySignInDonePath,
		rowUID:     uid,
	}, nil
}

// azureARMCapture is the capture an Azure CLI row serves, keyed by the row's
// id. It refuses an id that is not usable as a store name.
func azureARMCapture(componentID string) (entraCapture, error) {
	if !adoEntraValidRowID(componentID) {
		return entraCapture{}, fmt.Errorf("azure cli sign-in: component id %q is not usable as a store name", componentID)
	}
	return entraCapture{
		kind:       entraKindAzureARM,
		scopes:     []string{azureARMAudience},
		secretName: cliSecretName(componentID, providerEntraPart),
		donePath:   azureCLISignInDonePath,
		rowUID:     componentID,
	}, nil
}

// oneResource reports whether this kind's ceiling is one resource asked for as
// `<resource>/.default`, rather than Azure DevOps' exact scope literals.
func (c entraCapture) oneResource() bool {
	return c.kind == entraKindAzureFoundry || c.kind == entraKindAzureARM
}

// label is the lower-case name this kind's errors and logs use.
func (c entraCapture) label() string {
	switch c.kind {
	case entraKindAzureFoundry:
		return "azure foundry"
	case entraKindAzureARM:
		return "azure cli"
	case entraKindEndpointOIDC:
		return "endpoint oidc"
	}
	return "azure devops"
}

// logRow is the row key a log line names.
func (c entraCapture) logRow() string { return c.rowUID }

// auditProvider is the `provider` label this kind's audit and mask rows carry.
func (c entraCapture) auditProvider() string {
	if c.oneResource() || c.kind == entraKindEndpointOIDC {
		return string(c.kind)
	}
	return adoEntraProviderPrefix
}

// checkRequested holds a requested scope set to this kind's policy. It runs at
// validation, at login-request parsing (Azure DevOps) and at every redemption.
func (c entraCapture) checkRequested(scopes []string) error {
	if !c.oneResource() {
		return adoEntraCheckRequestedScopes(scopes)
	}
	return azureCheckRequestedScopes(c.scopes, scopes)
}

// azureCheckRequestedScopes admits exactly the audience literal the capture
// names plus the two identity scopes, and refuses every other `.default`: a
// `.default` for a different resource is a request for everything the
// application holds there, the opposite of a one-resource capture.
func azureCheckRequestedScopes(ceiling, scopes []string) error {
	if len(scopes) == 0 {
		return fmt.Errorf("a request must name the scopes it needs")
	}
	if len(scopes) > adoEntraMaxScopes {
		return fmt.Errorf("a request may name at most %d scopes", adoEntraMaxScopes)
	}
	for _, sc := range scopes {
		switch {
		case sc == "" || strings.ContainsAny(sc, " \t\r\n"):
			return fmt.Errorf("scope %q is not a single scope value", sc)
		case sc == entraOfflineAccessScope || sc == entraOpenIDScope:
		case slices.Contains(ceiling, sc):
		case sc == ".default" || strings.HasSuffix(sc, azureDefaultSuffix):
			return fmt.Errorf("refusing to request %q: an Azure sign-in names exactly the audience its route needs", sc)
		default:
			return fmt.Errorf("scope %q is not the audience this sign-in is captured for", sc)
		}
	}
	return nil
}

// capturedScopes is what a capture stores from a token response's granted
// scope string. Azure DevOps keeps the exact-literal intersect with the
// ceiling. A one-resource kind keeps the granted entries under the audience's
// resource, never the `.default` literal; none means the grant is unusable.
func (c entraCapture) capturedScopes(granted string) []string {
	if c.kind == entraKindEndpointOIDC {
		// RFC 6749 section 5.1: a response with no scope granted the requested set.
		if fields := strings.Fields(granted); len(fields) > 0 {
			return slices.Compact(slices.Sorted(slices.Values(fields)))
		}
		return slices.Clone(c.scopes)
	}
	if !c.oneResource() {
		return adoCaptureScopes(granted, c.scopes)
	}
	prefix := azureAudienceResource(c.scopes[0]) + "/"
	var out []string
	for _, sc := range strings.Fields(granted) {
		if strings.HasPrefix(sc, prefix) && sc != prefix && !strings.HasSuffix(sc, azureDefaultSuffix) && !slices.Contains(out, sc) {
			out = append(out, sc)
		}
	}
	return out
}

// consentCovers reports the first wanted scope the stored consent does not
// cover. Azure DevOps wants each literal in the blob. A one-resource kind wants
// the blob to hold at least one scope under the audience's resource.
func (c entraCapture) consentCovers(blobScopes, wanted []string) (string, bool) {
	if c.kind == entraKindEndpointOIDC {
		return "", true // the refresh names no scope, so the authority decides what it still covers
	}
	if !c.oneResource() {
		for _, want := range wanted {
			if !slices.Contains(blobScopes, want) {
				return want, false
			}
		}
		return "", true
	}
	prefix := azureAudienceResource(c.scopes[0]) + "/"
	if !slices.ContainsFunc(blobScopes, func(sc string) bool { return strings.HasPrefix(sc, prefix) }) {
		return c.scopes[0], false
	}
	return "", true
}

// sendsScopeOnRefresh reports whether a refresh names a scope. Every Entra kind does; an endpoint sign-in
// does not, and an authority that omits `scope` from a response (RFC 6749 section 5.1) granted the set the
// sign-in was captured with (omittedScopeIsGranted).
func (c entraCapture) sendsScopeOnRefresh() bool { return c.kind != entraKindEndpointOIDC }

// omittedScopeIsGranted reports whether a token response with no `scope` is read as the requested set.
// Only for an endpoint sign-in: an Entra response with none is refused.
func (c entraCapture) omittedScopeIsGranted() bool { return c.kind == entraKindEndpointOIDC }

// entraCodeGrant is an authorization code redeemed into a blob that is ready to
// store, with what the masking that follows the store needs.
type entraCodeGrant struct {
	blob       adoEntraBlob
	generation int64
	access     string
}

// redeemEntraCode is the middle of a row-keyed capture's callback, between its
// own checks of the row and its own store. In this order: it exchanges the
// code, puts both tokens on record for masking before anything can log them,
// binds the id_token to the session's subject, and keeps the scopes ec's policy
// admits. A non-empty reason is a refusal with that fixed code, and nothing is
// stored either way.
func (s *Server) redeemEntraCode(ctx context.Context, cfg ADOEntraConfig, ec entraCapture, subject, code, verifier, nonce string) (entraCodeGrant, string) {
	generation, err := s.cfg.MaskRegistry.GlobalGeneration(ctx)
	if err != nil {
		return entraCodeGrant{}, reasonStoreError
	}
	resp, err := s.postEntraToken(ctx, cfg, ec, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {cfg.RedirectURL},
	})
	if err != nil {
		reason := adoCaptureErrorCode(err)
		slog.WarnContext(ctx, "wardynd: the "+ec.label()+" sign-in code exchange failed",
			slog.String("row", ec.rowUID), slog.String("reason", reason), slog.Any("err", err))
		return entraCodeGrant{}, reason
	}
	now := s.cfg.Now()
	accessExpiry := now.Add(time.Duration(resp.ExpiresIn) * time.Second).UTC()
	if err := s.cfg.MaskRegistry.MergeGlobalUntil(generation, subject, ec.secretName, accessExpiry, []byte(resp.AccessToken), []byte(resp.RefreshToken)); err != nil {
		slog.ErrorContext(ctx, "wardynd: the Azure sign-in tokens could not be recorded for masking",
			slog.String("row", ec.rowUID), slog.Any("err", err))
		return entraCodeGrant{}, reasonStoreError
	}
	if reason, ok := s.bindEntraIdentity(ctx, cfg, ec, resp.IDToken, nonce, subject); !ok {
		slog.WarnContext(ctx, "wardynd: "+ec.label()+" sign-in identity binding failed",
			slog.String("row", ec.rowUID), slog.String("reason", reason))
		// Only an endpoint sign-in can tell "another authority" from "another person": the console binding
		// compares the issuer before the person.
		if ec.oidc != nil && reason == endpointOIDCWrongIssuerRefusal {
			return entraCodeGrant{}, reasonEndpointOIDCIssuerMismatch
		}
		return entraCodeGrant{}, reasonADOCallbackIdentityBinding
	}
	granted := ec.capturedScopes(resp.Scope)
	if resp.RefreshToken == "" || len(granted) == 0 {
		return entraCodeGrant{}, reasonADOCallbackUnusableGrant
	}
	blob := adoEntraBlob{
		RefreshToken: resp.RefreshToken,
		Scopes:       granted,
		ExpiresAt:    accessExpiry,
		TenantID:     cfg.TenantID,
		ClientID:     cfg.ClientID,
		Subject:      subject,
		CapturedAt:   now.UTC(),
		Source:       adoEntraSourceSignIn,
	}
	if ec.oidc != nil {
		blob.TenantID, blob.Issuer, blob.IDPSubject = "", ec.oidc.issuer, ec.oidc.idpSub
	}
	return entraCodeGrant{generation: generation, access: resp.AccessToken, blob: blob}, ""
}

// bindEntraIdentity binds the identity token to the session's subject under this kind's rule: an endpoint
// sign-in's (bindEndpointOIDCIdentity), or the console application's for every Entra kind.
func (s *Server) bindEntraIdentity(ctx context.Context, cfg ADOEntraConfig, ec entraCapture, rawIDToken, nonce, subject string) (string, bool) {
	if ec.oidc != nil {
		return s.bindEndpointOIDCIdentity(ctx, ec.oidc, rawIDToken, nonce, subject)
	}
	return s.bindADOEntraIdentity(ctx, cfg, rawIDToken, nonce, subject)
}

// retireReplacedEntraTokens runs once a capture's blob is stored. The exchange
// already put both tokens on record; this retires what the sign-in replaced,
// and a failure leaves the old values masked longer.
func (s *Server) retireReplacedEntraTokens(ctx context.Context, subject string, ec entraCapture, g entraCodeGrant) {
	if err := s.cfg.MaskRegistry.AddGlobalUntil(g.generation, subject, ec.secretName, s.cfg.Now(), g.blob.ExpiresAt, []byte(g.access), []byte(g.blob.RefreshToken)); err != nil {
		slog.WarnContext(ctx, "wardynd: the replaced Azure sign-in tokens could not be retired", slog.String("row", ec.rowUID), slog.Any("err", err))
	}
}
