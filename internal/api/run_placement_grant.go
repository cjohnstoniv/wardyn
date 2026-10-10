// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// createDispatchGrant covers every late provider/capture/redirect writer.
// Operator provenance is explicit: a colliding personal secret name cannot
// turn an operator redirect into an own credential. Remote writes are unchanged.
func (s *Server) createDispatchGrant(ctx context.Context, run types.AgentRun, g types.CredentialGrant, operatorHeld bool) (types.CredentialGrant, error) {
	if run.Placement == "" || run.Placement == types.PlacementRemote {
		return s.cfg.Store.CreateGrant(ctx, g)
	}
	if run.Placement != types.PlacementLocal || g.RunID != run.ID {
		return types.CredentialGrant{}, fmt.Errorf("unclassified dispatch grant placement or run")
	}
	if operatorHeld {
		return types.CredentialGrant{}, &placement.Refusal{Reason: placement.ReasonPlacementCredential, Field: "dispatch grant", Detail: "operator delivery is not implemented"}
	}
	origin, ref := s.localGrantOrigin(ctx, runIdentitySubject(ctx, run.CreatedBy), g.Spec)
	if ref != nil || origin.Class != placement.ClassOwn || !origin.Stored || !origin.OwnNamespace {
		return types.CredentialGrant{}, &placement.Refusal{Reason: placement.ReasonPlacementCredential, Field: "dispatch grant", Detail: "credential is not available in its owner's namespace"}
	}
	g.Spec.OwnerOnly = true
	g.Delivery = types.GrantDeliveryOwn
	return s.cfg.Store.CreateGrant(ctx, g)
}

// localCapturedGrantOrigin inspects the raw snapshot rather than permitting
// an opaque JSON child to escape the closed schema. Only known, owner-bound
// provider captures are own. Minted ADO credentials remain brokered even when
// the captured sign-in is held by a person.
func (s *Server) localCapturedGrantOrigin(ctx context.Context, owner string, g types.GrantSpec) (placement.CredentialOrigin, bool, error) {
	if g.Kind != types.GrantAPIKey {
		return placement.CredentialOrigin{}, false, nil
	}
	var outer struct {
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(g.Scope, &outer); err != nil {
		return placement.CredentialOrigin{}, true, err
	}
	if len(outer.Snapshot) == 0 {
		return placement.CredentialOrigin{}, false, nil
	}
	rule, err := injectionRuleFromScope(g.Scope)
	if err != nil {
		return placement.CredentialOrigin{}, true, err
	}
	sc, err := s.siteConfigForDispatch(ctx)
	if err != nil {
		return placement.CredentialOrigin{}, true, err
	}
	name, err := localCapturedSecretName(sc, owner, rule.SecretName, outer.Snapshot)
	if err != nil {
		return placement.CredentialOrigin{}, true, err
	}
	own := owner != "" && name != "" && s.ownsSecretMemoized(ctx, owner, name)
	return placement.CredentialOrigin{GrantKind: g.Kind, Class: placement.ClassOwn, Delivery: placement.ClassAPIKey, Stored: true, OwnNamespace: own, OwnerOnly: own}, true, nil
}

func localCapturedSecretName(sc types.SiteConfig, owner, sentinel string, raw json.RawMessage) (string, error) {
	if sentinel == types.ADOEntraAccessTokenSecret {
		return "", fmt.Errorf("captured ADO token delivery is not implemented")
	}
	var sn providerGrantSnapshot
	var part string
	if sentinel == types.AWSSSOAccessTokenSecret {
		var aws awsSSOScopeSnapshot
		if err := closedLocalSnapshot(raw, &aws); err != nil || !aws.authored() || aws.CredentialSource != string(types.CredentialSourcePerUser) || aws.Mechanism != string(types.ModelProviderBedrockSSO) {
			return "", fmt.Errorf("unclassified AWS capture snapshot")
		}
		sn.ProviderUID, sn.OwnerSubject, part = aws.ProviderUID, aws.OwnerSubject, providerSSOPart
	} else {
		var azure azureGrantSnapshot
		if closedLocalSnapshot(raw, &sn) != nil {
			if err := closedLocalSnapshot(raw, &azure); err != nil || !azure.authored() {
				return "", fmt.Errorf("unclassified provider snapshot")
			}
			sn.ProviderUID, sn.OwnerSubject, part = azure.ProviderUID, azure.OwnerSubject, providerEntraPart
		}
	}
	if owner == "" || sn.OwnerSubject != owner || sn.ProviderUID == "" {
		return "", fmt.Errorf("capture owner is not the run owner")
	}
	if sc.ModelProviders == nil {
		return "", fmt.Errorf("capture provider is not configured")
	}
	for _, p := range sc.ModelProviders.Providers {
		if p.UID != sn.ProviderUID || p.Disabled {
			continue
		}
		if part == "" {
			switch p.Kind {
			case types.ModelProviderAnthropicSubscription:
				part = providerOAuthPart
			case types.ModelProviderAnthropicAPIKey, types.ModelProviderOpenAIAPIKey, types.ModelProviderCustomEndpoint, types.ModelProviderBedrockBearer:
				part = providerKeyPart
			default:
				return "", fmt.Errorf("unclassified provider capture kind")
			}
		} else if (part == providerSSOPart && p.Kind != types.ModelProviderBedrockSSO) || (part == providerEntraPart && p.Kind != types.ModelProviderAzureFoundry) {
			return "", fmt.Errorf("capture does not match provider kind")
		}
		name := providerSecretName(p.UID, part)
		if sentinel != types.AWSSSOAccessTokenSecret && sentinel != name {
			return "", fmt.Errorf("capture does not match provider secret")
		}
		return name, nil
	}
	return "", fmt.Errorf("capture provider is not available")
}

func closedLocalSnapshot(raw json.RawMessage, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("snapshot has trailing data")
	}
	return nil
}
