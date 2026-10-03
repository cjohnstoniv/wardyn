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

import (
	"fmt"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// entraKind names which scope policy a capture runs under.
type entraKind string

const (
	entraKindADO          entraKind = "azure_devops"
	entraKindAzureFoundry entraKind = "azure_foundry"
)

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

// label is the lower-case name this kind's errors and logs use.
func (c entraCapture) label() string {
	if c.kind == entraKindAzureFoundry {
		return "azure foundry"
	}
	return "azure devops"
}

// logRow is the row key a log line names.
func (c entraCapture) logRow() string { return c.rowUID }

// auditProvider is the `provider` label this kind's audit and mask rows carry.
func (c entraCapture) auditProvider() string {
	if c.kind == entraKindAzureFoundry {
		return string(entraKindAzureFoundry)
	}
	return adoEntraProviderPrefix
}

// checkRequested holds a requested scope set to this kind's policy. It runs at
// validation, at login-request parsing (Azure DevOps) and at every redemption.
func (c entraCapture) checkRequested(scopes []string) error {
	if c.kind != entraKindAzureFoundry {
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
// ceiling. azure_foundry keeps the granted entries under the audience's
// resource, never the `.default` literal; none means the grant is unusable.
func (c entraCapture) capturedScopes(granted string) []string {
	if c.kind != entraKindAzureFoundry {
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
// cover. Azure DevOps wants each literal in the blob. azure_foundry wants the
// blob to hold at least one scope under the audience's resource.
func (c entraCapture) consentCovers(blobScopes, wanted []string) (string, bool) {
	if c.kind != entraKindAzureFoundry {
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
