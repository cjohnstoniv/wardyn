// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// CredentialOrigin is control-plane provenance for one actual dispatch field.
// The caller resolves it from admitted owner/provider/grant snapshots, never
// from runner assertions. Stored credentials require proof of the own namespace.
type CredentialOrigin struct {
	Class        Class
	Delivery     string
	GrantKind    types.GrantKind
	Stored       bool
	OwnNamespace bool
	OwnerOnly    bool
}

// CredentialIntent records a resolved credential-producing configuration before mint.
// Its Field uses the same canonical paths as an actual SandboxSpec value.
type CredentialIntent struct {
	Field  string
	Origin CredentialOrigin
}

// LocalPlan includes the final spec and the non-wire authoring facts a spec
// alone cannot prove. Origins keys are the paths returned by CredentialPaths.
// Missing provenance refuses, including fields added by late dispatch phases.
type LocalPlan struct {
	Spec                       runner.SandboxSpec
	Origins                    map[string]CredentialOrigin
	CredentialIntents          []CredentialIntent
	OrgConfigKeys              []string
	VerifiedLocalPaths         map[string]bool
	UpstreamProxySecretRef     string
	TrustedOutput              bool
	SelfDefinedComponents      []string
	LocalSelfDefinedComponents bool
	Delivery                   DeliveryPolicy
}

// Refusal names the actual field that makes local execution ineligible.
type Refusal struct {
	Reason Reason
	Field  string
	Detail string
}

func (r *Refusal) Error() string                          { return r.Field + ": " + r.Detail }
func refuse(reason Reason, field, detail string) *Refusal { return &Refusal{reason, field, detail} }

// LocalEligibility classifies the complete resolved plan with the canonical
// table. It does not select placement or enable delivery. H12/H13 must supply
// enforcement before an org-held class can use any configured delivery mode.
// The returned spec is a copy with operator network/inspection/config removed.
func LocalEligibility(p LocalPlan) (runner.SandboxSpec, *Refusal) {
	if fields := Unclassified(); len(fields) != 0 {
		return runner.SandboxSpec{}, refuse(ReasonPlacementCredential, fields[0], "unclassified dispatch field")
	}
	if p.TrustedOutput {
		return runner.SandboxSpec{}, refuse(ReasonPlacementTrustedOutput, "trusted_output", "this operation requires the organisation's substrate")
	}
	if len(p.SelfDefinedComponents) != 0 && !p.LocalSelfDefinedComponents {
		return runner.SandboxSpec{}, refuse(ReasonPlacementComponentSelfDefine, p.SelfDefinedComponents[0], "local_self_defined_components is not permitted by the profile")
	}
	if p.UpstreamProxySecretRef != "" {
		return runner.SandboxSpec{}, refuse(ReasonPlacementCredential, "upstream_proxy_secret_ref", "the runner must use its own upstream proxy configuration")
	}
	for i, g := range p.Spec.ProxyConfig.Policy.EligibleGrants {
		path := indexed("ProxyConfig.Policy.EligibleGrants", i)
		origin := p.Origins[path]
		if origin.GrantKind != g.Kind || (origin.Class == ClassOwn && !g.OwnerOnly) {
			return runner.SandboxSpec{}, refuse(ReasonPlacementCredential, path, "credential provenance does not match its actual grant")
		}
	}
	for _, path := range CredentialPaths(p.Spec) {
		if r := classifyCredential(Entries, path, p.Origins[path], p.Delivery); r != nil {
			return runner.SandboxSpec{}, r
		}
	}
	for _, intent := range p.CredentialIntents {
		if r := classifyCredential(Entries, intent.Field, intent.Origin, p.Delivery); r != nil {
			return runner.SandboxSpec{}, r
		}
	}
	out, r := enforceTable(Entries, p)
	if r != nil {
		return runner.SandboxSpec{}, r
	}
	return out, nil
}

func indexed(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }

// matchingRows are the rows of the origin's class, the rows naming its exact
// delivery first: a row with no delivery applies to any origin of the class
// only after every exact row has had its say, so it can never shadow a
// refusing row (well-formedness also forbids the two sharing a class).
func matchingRows(rows []Entry, origin CredentialOrigin) []Entry {
	var exact, general []Entry
	for _, row := range rows {
		switch {
		case row.Class != origin.Class:
		case row.Delivery == "":
			general = append(general, row)
		case row.Delivery == origin.Delivery:
			exact = append(exact, row)
		}
	}
	return append(exact, general...)
}

func classifyCredential(entries entrySource, path string, origin CredentialOrigin, delivery DeliveryPolicy) *Refusal {
	if origin.Class == "" {
		return refuse(ReasonPlacementCredential, path, "credential provenance is unknown")
	}
	// Grant records carry a kind-specific route rather than one sandbox field.
	field := strings.SplitN(strings.TrimPrefix(path, "SandboxSpec."), "[", 2)[0]
	st := StructSandboxSpec
	if strings.HasPrefix(path, "ProxyConfig.") {
		st = StructProxyConfig
		field = strings.SplitN(strings.TrimPrefix(path, "ProxyConfig."), "[", 2)[0]
	}
	if field == "Policy.EligibleGrants" {
		st, field = StructGrantKind, string(origin.GrantKind)
	}
	for _, row := range matchingRows(entries(st, field), origin) {
		switch row.Rule {
		case RuleAllow, RuleOwnerOnlyOwn:
			if origin.Class == ClassOwn && !origin.OwnNamespace {
				return refuse(ReasonPlacementCredential, path, "credential requires proof of its owner namespace")
			}
			if origin.Class == ClassOwn && origin.Stored && (!origin.OwnNamespace || !origin.OwnerOnly) {
				return refuse(ReasonPlacementCredential, path, "stored credential requires its owner's namespace and OwnerOnly")
			}
			if row.Rule == RuleOwnerOnlyOwn && !origin.Stored {
				continue
			}
			return nil
		case RuleDelivery, RuleViaOrgRefuse, RuleNotConfigured:
			mode := delivery.For(row.Delivery).Mode
			return refuse(ReasonPlacementCredential, path, fmt.Sprintf("%s credential is unavailable on a local runner (delivery %s has no implemented authority)", row.Delivery, mode))
		default:
			return refuse(ReasonPlacementCredential, path, "credential row has a rule this server does not implement")
		}
	}
	return refuse(ReasonPlacementCredential, path, "credential variant has no permitted local rule")
}
