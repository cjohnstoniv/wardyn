// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
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
	// ResidentOptIn is an Azure DevOps connection's admin opt-in to
	// runner_resident (types.ADOEntraConfig.RunnerResident); off by default.
	ResidentOptIn bool
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
	// Posture is the candidate runner's stored, self-reported posture; nil when
	// it never reported one.
	Posture *types.RunnerPosture
	// Trust is the candidate runner's stored trust class, which Delivery is read
	// at (DeliveryPolicy.ForTrust); "" (unknown) refuses every class but via_org.
	Trust types.RunnerTrustClass
	// ConfinementFloor is the run's blast-radius floor
	// (composer.RequiredConfinementFloor); "" when there is none.
	ConfinementFloor types.ConfinementClass
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
// table. It does not select placement. An org-held class is admitted only by a
// configured via_org mode on a row of the via_org shape; runner_resident (H13)
// still refuses. The returned spec is a copy with operator network/inspection/
// config removed and each via_org grant reduced to its ViaOrgRoutes entry.
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
		if r := classifyCredential(Entries, path, p.Origins[path], p, credentialElement(p.Spec, path)); r != nil {
			return runner.SandboxSpec{}, r
		}
	}
	for _, intent := range p.CredentialIntents {
		if r := classifyCredential(Entries, intent.Field, intent.Origin, p, reflect.Value{}); r != nil {
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

// trustName is a trust class as a refusal names it.
func trustName(t types.RunnerTrustClass) string {
	if !t.Valid() {
		return "unknown"
	}
	return string(t)
}

// viaOrgAdmits is the via_org gate (OD-12, design §7.2-§7.3): a row of the
// via_org shape whose class the stored policy delivers via_org, and whose class
// may be. Every other configured mode still refuses here until its delivery is built.
func viaOrgAdmits(row Entry, mode Mode) bool {
	return row.ViaOrg && row.Rule != RuleNotConfigured && mode == ModeViaOrg && slices.Contains(classModes[row.Delivery], ModeViaOrg)
}

// ViaOrgGrant reports whether an org-held grant of origin is delivered via_org
// under policy on a runner of trust class trust: the same table rows and gate
// LocalEligibility applies to it.
func ViaOrgGrant(origin CredentialOrigin, policy DeliveryPolicy, trust types.RunnerTrustClass) bool {
	return origin.Class != ClassOwn && origin.Class != "" &&
		classifyCredential(Entries, "ProxyConfig.Policy.EligibleGrants", origin, LocalPlan{Delivery: policy, Trust: trust}, reflect.Value{}) == nil &&
		policy.ForTrust(origin.Delivery, trust).Mode == ModeViaOrg
}

// viaOrgPath reports whether the admitted credential at path is delivered
// via_org. Classification has already admitted every path the strippers see,
// and via_org is the only admission of an org-held class.
func viaOrgPath(p LocalPlan, path string) bool {
	o, ok := p.Origins[path]
	return ok && o.Class != ClassOwn && o.Class != "" && p.Delivery.ForTrust(o.Delivery, p.Trust).Mode == ModeViaOrg
}

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

// classifyCredential judges one credential. leaf is the dispatched value at
// path; it is invalid for an intent, which has no value yet.
func classifyCredential(entries entrySource, path string, origin CredentialOrigin, p LocalPlan, leaf reflect.Value) *Refusal {
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
			mode := p.Delivery.ForTrust(row.Delivery, p.Trust).Mode
			if viaOrgAdmits(row, mode) {
				return nil
			}
			if mode == ModeRunnerResident && row.Rule == RuleDelivery {
				if r := ResidentAdmits(origin, p, path); r != nil {
					return r
				}
				return residentValueRefusal(path, row.Delivery, st == StructGrantKind, leaf)
			}
			return refuse(ReasonPlacementCredential, path, notDeliveredDetail(row.Delivery, p.Trust, mode))
		default:
			return refuse(ReasonPlacementCredential, path, "credential row has a rule this server does not implement")
		}
	}
	return refuse(ReasonPlacementCredential, path, "credential variant has no permitted local rule")
}

// notDeliveredDetail is the refusal of a class no admitted mode delivers to a
// runner of trust: it names the class, the trust class and the delivery policy
// that applied.
func notDeliveredDetail(class string, trust types.RunnerTrustClass, mode Mode) string {
	at := class + " credential is not delivered to a local runner of trust class " + trustName(trust)
	switch {
	case mode == ModeViaOrg: // a via_org-shaped row would have been admitted (viaOrgAdmits)
		return at + ": via_org delivery of this credential is not built yet (delivery policy: via_org)"
	case !trust.Valid():
		return at + " (a runner of unknown trust class gets via_org delivery only)"
	case mode == ModeRefuse || mode == "":
		return at + " (delivery policy: refuse)"
	}
	return at + " (delivery policy: " + string(mode) + ")"
}

// ResidentAdmits is the runner_resident gate (OD-12, design §7.4, Q7) for a
// credential of origin, whose delivery class is origin.Delivery: the class is
// delivered runner_resident under p's policy and may be, an Azure DevOps
// connection opted in, the run has no blast-radius floor (a runner-asserted
// class never satisfies one for a value the runner holds), and the runner's
// posture meets the class's requirement. LocalEligibility applies it at create,
// dispatch and revive; the org's delivery applies it again to each value,
// against the policy and posture of that moment. path names the refused field.
func ResidentAdmits(origin CredentialOrigin, p LocalPlan, path string) *Refusal {
	class := origin.Delivery
	cp := p.Delivery.ForTrust(class, p.Trust)
	switch {
	case !slices.Contains(classModes[class], ModeRunnerResident):
		return refuse(ReasonPlacementCredential, path, class+" credential is never runner_resident")
	case cp.Mode != ModeRunnerResident:
		return refuse(ReasonPlacementCredential, path, class+" credential is not delivered runner_resident under the current delivery policy")
	case class == ClassADOMintedPAT && !origin.ResidentOptIn:
		return refuse(ReasonPlacementCredential, path, "the Azure DevOps connection has not opted in to runner_resident")
	case p.ConfinementFloor != "":
		return refuse(ReasonRunnerPostureUnmet, path, "confinement_class: a runner-asserted class never meets the "+string(p.ConfinementFloor)+" floor for a runner_resident credential")
	}
	switch unmet := PostureUnmetAll(cp.RequirePosture, p.Posture); {
	case len(unmet) == 0:
	case p.Posture == nil:
		return refuse(ReasonRunnerPostureUnmet, path, "posture: no reported posture to judge. At launch no runner is chosen yet, and a runner that has not reported its posture meets no requirement, so a delivery policy with require_posture refuses (requires "+cp.RequirePosture.requires()+")")
	default:
		return refuse(ReasonRunnerPostureUnmet, path, strings.Join(unmet, ", ")+": the runner's reported posture does not meet "+class+"'s requirement (requires "+cp.RequirePosture.requires()+")")
	}
	return nil
}

// residentValueRefusal keeps an admitted runner_resident value off every
// channel but DeliverResident: the dispatched leaf must already be its
// placeholder. A grant's eligibility record names a scope, never a value, and is
// admitted for a class whose resident delivery is built (residentBuilt): the
// org's deliver step authors that grant's placeholder at dispatch. A record, a
// placeholder or an intent (Azure DevOps) of any other class stays refused, so
// classifyCredential never admits an unbuilt class, whatever the caller (via_org
// admission included) concludes from nil.
func residentValueRefusal(path, class string, record bool, leaf reflect.Value) *Refusal {
	switch {
	case !residentBuilt[class]:
		return refuse(ReasonPlacementCredential, path, "runner_resident delivery of this credential is not built yet")
	case record:
		return nil
	case !leaf.IsValid():
		return refuse(ReasonPlacementCredential, path, "runner_resident delivery of this credential is not built yet")
	case !residentPlaceholderLeaf(leaf):
		return refuse(ReasonPlacementCredential, path, "a runner_resident value is dispatched only as its placeholder, never inline")
	}
	return nil
}

// residentPlaceholderLeaf reports whether v is a placeholder: a string, a byte
// slice, a managed file whose Content is one, or a resident mint whose Token is
// one.
func residentPlaceholderLeaf(v reflect.Value) bool {
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	switch {
	case v.Kind() == reflect.String:
		_, ok := ResidentGrantID(v.String())
		return ok
	case v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8:
		_, ok := ResidentGrantID(string(v.Bytes()))
		return ok
	case v.Kind() == reflect.Struct && v.Type() == reflect.TypeFor[runner.ManagedFile]():
		return residentPlaceholderLeaf(v.FieldByName("Content"))
	case v.Kind() == reflect.Struct && v.Type() == reflect.TypeFor[proxy.ResidentMint]():
		// A mint the runner answers carries the cap it is refused at.
		return !v.FieldByName("ExpiresAt").Interface().(time.Time).IsZero() && residentPlaceholderLeaf(v.FieldByName("Token"))
	}
	return false
}

// credentialElement is the value at a CredentialPaths path; invalid when the
// path names nothing in s.
func credentialElement(s runner.SandboxSpec, path string) reflect.Value {
	root := reflect.ValueOf(s)
	rest, ok := strings.CutPrefix(path, "SandboxSpec.")
	if !ok {
		if rest, ok = strings.CutPrefix(path, "ProxyConfig."); !ok {
			return reflect.Value{}
		}
		root = root.FieldByName("ProxyConfig")
	}
	field, key, indexed := strings.Cut(rest, "[")
	v := root.FieldByName(field)
	if !v.IsValid() || !indexed {
		return v
	}
	key = strings.TrimSuffix(key, "]")
	switch v.Kind() {
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if fmt.Sprint(k.Interface()) == key {
				return v.MapIndex(k)
			}
		}
	case reflect.Slice, reflect.Array:
		if i, err := strconv.Atoi(key); err == nil && i >= 0 && i < v.Len() {
			return v.Index(i)
		}
	}
	return reflect.Value{}
}

// CredentialProxyField reports whether the table classifies a ProxyConfig field
// as credential-bearing: the only fields a runner fills a resident value into.
func CredentialProxyField(field string) bool {
	return rulesOf(Entries(StructProxyConfig, field)).credential
}
