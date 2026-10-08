// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// componentSecretReserved reports whether name is one Wardyn manages itself
// and no component may name: every name the secrets API refuses to store
// (platform keys, captured sign-ins, provider credentials), every name a
// credential sink refuses to resolve, and the retired model credentials. A
// person's own namespace lists their captured sign-in blobs beside the secrets
// they stored, so "they own it" does not make one of these theirs to deliver;
// dispatch would strip or refuse such a grant, and the run would start without
// the credential and without a reason.
func componentSecretReserved(name string) bool {
	return secretsAPIReserved(name) || sinkReservedSecret(name) || slices.Contains(retiredModelCredentialNames, name)
}

// componentDefinitionProblem is the gate's shape check of one attached
// component, run for a stored row exactly as for an inline one: a row saved
// under an older rule is never admitted on trust. It returns the field at
// fault and what is wrong with it, or "".
//
// types' Validate carries every rule a definition can be held to on its own
// (a person's hosts are DNS names, a header host is bare, shared and
// plain_http are an organisation's, a file name is one path element); the
// reserved-name rule needs this package's tables.
func componentDefinitionProblem(c attachedComponent) string {
	def := c.snapshot.Definition
	if err := def.Validate(proxy.ValidDomainEntry, c.snapshot.SelfDefined); err != nil {
		return err.Error()
	}
	for i, sec := range def.Secrets {
		if componentSecretReserved(sec.SecretName) {
			return fmt.Sprintf("secrets[%d].secret_name: %q is managed by Wardyn", i, sec.SecretName)
		}
	}
	return ""
}

// componentModelEnvProblem refuses a variable a model credential rides in or a
// model-provider arm sets: a run's model credential comes only from its model
// provider, and a component writing one of these names would replace or
// re-point it.
func componentModelEnvProblem(def types.ComponentDefinition) string {
	for i, sec := range def.Secrets {
		if sec.Delivery.Mode == types.ComponentDeliveryEnv && modelEnvNames[sec.Delivery.Var] {
			return fmt.Sprintf("secrets[%d].delivery.var: %q is set by the run's model provider", i, sec.Delivery.Var)
		}
	}
	for _, k := range sortedKeys(def.Config) {
		if modelEnvNames[k] {
			return fmt.Sprintf("config: key %q is set by the run's model provider", k)
		}
	}
	return ""
}

// componentResidentSecret is the first secret a definition delivers INTO the
// sandbox (an env var or a file), or -1.
func componentResidentSecret(def types.ComponentDefinition) int {
	return slices.IndexFunc(def.Secrets, func(sec types.ComponentSecret) bool {
		return sec.Delivery.Mode != types.ComponentDeliveryHeader
	})
}

// residentTargets is every place a run already delivers a value into the
// sandbox by name: the variables and files of the grants on its spec. The gate
// adds each component's own as it admits them, so two deliveries into one
// variable or one file — which dispatch would resolve by silently skipping the
// second — are refused where the person can still edit the request.
func residentTargets(spec types.RunPolicySpec) map[string]bool {
	taken := map[string]bool{}
	for _, g := range spec.EligibleGrants {
		switch g.Kind {
		case types.GrantEnvSecret:
			if name, _, err := envSecretScopeFields(g.Scope); err == nil {
				taken["env "+name] = true
			}
		case types.GrantFileSecret:
			if file, _, err := fileSecretScopeFields(g.Scope); err == nil {
				taken["file "+file] = true
			}
		}
	}
	return taken
}

// claimResidentTargets records def's variables, files and config keys in
// taken, or returns the field whose place is already taken and leaves taken as
// it was. Within one definition Validate has already refused a repeat, so a
// clash here is always with another part of the run.
func claimResidentTargets(def types.ComponentDefinition, taken map[string]bool) string {
	var claims []string
	for i, sec := range def.Secrets {
		var target string
		switch sec.Delivery.Mode {
		case types.ComponentDeliveryEnv:
			target = "env " + sec.Delivery.Var
		case types.ComponentDeliveryFile:
			target = "file " + sec.Delivery.File
		default:
			continue
		}
		if taken[target] {
			return fmt.Sprintf("secrets[%d].delivery: another part of this run already delivers to the same place", i)
		}
		claims = append(claims, target)
	}
	for _, k := range sortedKeys(def.Config) {
		if taken["env "+k] {
			return fmt.Sprintf("config: key %q is already set by another part of this run", k)
		}
		claims = append(claims, "env "+k)
	}
	for _, c := range claims {
		taken[c] = true
	}
	return ""
}

// componentSecretsOwned is ownership before existence: every secret a
// component does not get from the organisation must be the caller's own. Only
// the caller's namespace is read, names only, and never the operator's on a
// person's run — so the answer for a name is the same whatever the operator
// holds under it, and the gate cannot be used to probe the operator's secret
// names. The secret's name is said even for an organisation's component: it
// is the name the person has to store.
func (s *Server) componentSecretsOwned(r *http.Request, adm *componentAdmission, c attachedComponent, i int) *componentRefusal {
	for si, sec := range c.snapshot.Definition.Secrets {
		if sec.Shared || s.callerOwnsSecret(r, adm, sec.SecretName) {
			continue
		}
		msg := fmt.Sprintf("components[%d]: %ssecrets[%d].secret_name: you have no secret named %q of your own. Store it under Your account first.",
			i, c.fieldPath(), si, sec.SecretName)
		if !c.snapshot.SelfDefined {
			msg = fmt.Sprintf("components[%d]: this component needs a secret of your own named %q. Store it under Your account first.", i, sec.SecretName)
		}
		return &componentRefusal{
			refusal: runError(http.StatusUnprocessableEntity, reasonComponentSecretNotOwned, msg),
			reason:  reasonComponentSecretNotOwned, ordinal: i, source: c.source, id: c.snapshot.ComponentID,
		}
	}
	return nil
}

// callerOwnsSecret reports whether the namespace an owner_only grant on this
// run will be read from holds name: the run identity's own rows, or — for a
// run the operator itself owns — the operator's, which is its own
// (grantReadOwner). The same namespace the sink reads, so a component the gate
// admits is one whose secret resolves.
func (s *Server) callerOwnsSecret(r *http.Request, adm *componentAdmission, name string) bool {
	if operatorOwnedRequest(r.Context()) {
		return s.operatorSecretNames(r.Context(), adm)[name]
	}
	return s.ownsSecretMemoized(r.Context(), adm.caller, name)
}

func (s *Server) operatorSecretNames(ctx context.Context, adm *componentAdmission) map[string]bool {
	if adm.operatorSecrets == nil {
		adm.operatorSecrets = s.presentSecretNames(ctx)
	}
	return adm.operatorSecrets
}

// componentSharedSecretsPresent is the existence half for a secret the
// organisation provides: the operator must have stored it. The sentence names
// the component only, never the secret — that name is the operator's. On the
// policy preview a missing one is recorded on the component instead.
func (s *Server) componentSharedSecretsPresent(r *http.Request, adm *componentAdmission, c attachedComponent, i int) (attachedComponent, *componentRefusal) {
	for _, sec := range c.snapshot.Definition.Secrets {
		if !sec.Shared || s.operatorSecretNames(r.Context(), adm)[sec.SecretName] {
			continue
		}
		if !adm.credentials {
			c.needsAdminSecret = true
			continue
		}
		return attachedComponent{}, &componentRefusal{
			refusal: runError(http.StatusUnprocessableEntity, reasonComponentSecretMissing,
				fmt.Sprintf("components[%d]: this component needs a secret your admin has not provided yet. Ask your admin.", i)),
			reason: reasonComponentSecretMissing, ordinal: i, source: c.source, id: c.snapshot.ComponentID,
		}
	}
	return c, nil
}
