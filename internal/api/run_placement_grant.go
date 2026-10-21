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
	if remotePlacement(run.Placement) {
		return s.cfg.Store.CreateGrant(ctx, g)
	}
	if run.Placement != types.PlacementLocal || g.RunID != run.ID {
		return types.CredentialGrant{}, fmt.Errorf("unclassified dispatch grant placement or run")
	}
	if operatorHeld {
		return types.CredentialGrant{}, &placement.Refusal{Reason: placement.ReasonPlacementCredential, Field: "dispatch grant", Detail: "operator delivery is not implemented"}
	}
	origin, ref := s.localGrantOrigin(ctx, runIdentitySubject(ctx, run.CreatedBy), g.Spec)
	if ref == nil && residentADOOrigin(origin) {
		if r, err := s.residentADOAdmits(ctx, run, origin); err != nil || r != nil {
			if r == nil {
				r = &placement.Refusal{Reason: placement.ReasonPlacementCredential, Field: "dispatch grant", Detail: "the delivery policy or the runner's posture could not be read"}
			}
			return types.CredentialGrant{}, r
		}
		g.Delivery = types.GrantDeliveryResident
		return s.cfg.Store.CreateGrant(ctx, g)
	}
	if ref != nil || origin.Class != placement.ClassOwn || !origin.Stored || !origin.OwnNamespace {
		return types.CredentialGrant{}, &placement.Refusal{Reason: placement.ReasonPlacementCredential, Field: "dispatch grant", Detail: "credential is not available in its owner's namespace"}
	}
	g.Spec.OwnerOnly = true
	g.Delivery = types.GrantDeliveryOwn
	return s.cfg.Store.CreateGrant(ctx, g)
}

// residentADOOrigin reports whether origin is an Azure DevOps token the
// organisation mints per run (localADOGrantOrigin), the one dispatch grant that
// may be recorded runner_resident.
func residentADOOrigin(origin placement.CredentialOrigin) bool {
	return origin.Class == placement.ClassBrokered && origin.Delivery == placement.ClassADOMintedPAT
}

// residentADOAdmits judges an Azure DevOps minted token's runner_resident
// delivery to run's runner: the delivery policy, the bound runner's trust
// class, owner kind and posture as stored now, and the connection's opt-in
// carried by origin. The run's blast-radius floor is judged by the dispatch
// classifier (classifyLocalDispatch), which re-judges the grant's injection
// rule with it. The runner's proxy then fetches the token over the relay, per
// resolve and at most every residentRejudgeEvery, and holds it in memory only;
// no value is handed to the runner. err is a read that failed: nothing was
// judged.
func (s *Server) residentADOAdmits(ctx context.Context, run types.AgentRun, origin placement.CredentialOrigin) (*placement.Refusal, error) {
	var p placement.LocalPlan
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err == nil && run.RunnerID == nil {
		err = fmt.Errorf("run %s has no runner", run.ID)
	}
	if err == nil {
		err = s.localPlanFacts(ctx, &p, egressBaselineOf(sc), run.RunnerID)
	}
	if err != nil {
		return nil, err
	}
	return placement.ResidentAdmits(origin, p, "ProxyConfig.Injection"), nil
}

// localCapturedGrantOrigin inspects the raw snapshot rather than permitting
// an opaque JSON child to escape the closed schema. Only known, owner-bound
// provider captures are own. An Azure DevOps grant is own for the owner's own
// PAT, and brokered for a token the organisation mints from the owner's
// captured sign-in; a bearer token is refused.
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
	if rule.SecretName == types.ADOEntraAccessTokenSecret {
		return s.localADOGrantOrigin(ctx, owner, outer.Snapshot)
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

// localADOGrantOrigin classifies one Azure DevOps lane grant from its snapshot
// with the lane's own decision, localADOOrigin: own only for the run owner's own
// PAT proven in their namespace. A minted_pat token is the organisation's,
// minted per run from the owner's captured sign-in: brokered ado_minted_pat,
// opted in only while its connection, as configured now, still is
// (ADOEntraConfig.RunnerResident) and still mints. A bearer token, or any other
// mode, has no local delivery and refuses.
func (s *Server) localADOGrantOrigin(ctx context.Context, owner string, raw json.RawMessage) (placement.CredentialOrigin, bool, error) {
	var sn adoEntraScopeSnapshot
	if err := closedLocalSnapshot(raw, &sn); err != nil {
		return placement.CredentialOrigin{}, true, err
	}
	switch types.ADOTokenMode(sn.TokenMode) {
	case types.ADOTokenModeMintedPAT:
		if owner == "" || sn.OwnerSubject != owner {
			return placement.CredentialOrigin{}, true, fmt.Errorf("ADO minted PAT is not for the run owner")
		}
		sc, err := s.siteConfigForDispatch(ctx)
		if err != nil {
			return placement.CredentialOrigin{}, true, err
		}
		row, _ := gitProviderRowByID(sc, sn.ProviderRowID)
		optIn := row.Entra != nil && !row.Disabled && row.Entra.RunnerResident && cmpTokenMode(row.Entra.TokenMode) == types.ADOTokenModeMintedPAT
		return placement.CredentialOrigin{GrantKind: types.GrantAPIKey, Class: placement.ClassBrokered, Delivery: placement.ClassADOMintedPAT, ResidentOptIn: optIn}, true, nil
	case types.ADOTokenModeOwnPAT:
		origin := s.localADOOrigin(ctx, owner, adoEntraRun{rowID: sn.ProviderRowID, owner: sn.OwnerSubject, tokenMode: types.ADOTokenModeOwnPAT})
		if origin.Class != placement.ClassOwn {
			return placement.CredentialOrigin{}, true, fmt.Errorf("ADO own PAT is not the run owner's")
		}
		origin.GrantKind, origin.Delivery = types.GrantAPIKey, placement.ClassAPIKey
		return origin, true, nil
	default:
		return placement.CredentialOrigin{}, true, fmt.Errorf("captured ADO token delivery is not implemented")
	}
}

func localCapturedSecretName(sc types.SiteConfig, owner, sentinel string, raw json.RawMessage) (string, error) {
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
