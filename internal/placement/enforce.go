// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// entrySource names the table rows of one field. Production passes Entries;
// a test passes a fixture table, which is how a row for a field that does not
// exist yet is proven to enforce.
type entrySource func(structName, field string) []Entry

// ruleSet is what the table says about one field, read from every row of it.
// Enforcement follows this and nothing else: a row added to Table is
// enforced without anyone editing a field list.
type ruleSet struct{ credential, gated, notSent, strip bool }

func rulesOf(rows []Entry) ruleSet {
	var r ruleSet
	for _, e := range rows {
		switch e.Rule {
		case RuleDelivery, RuleViaOrgRefuse, RuleNotConfigured, RuleOwnerOnlyOwn:
			r.credential = true
		case RuleRefuse, RuleBound:
			r.gated = true
		case RuleNotSent:
			r.notSent = true
		case RuleStrip:
			if e.Class != ClassComposite {
				r.strip = true
			}
		}
	}
	return r
}

// stripper narrows a laptop-bound copy for one RuleStrip row, keyed
// "Struct.Field[.Path]". A strip row with no stripper refuses.
type stripper func(p LocalPlan, s *runner.SandboxSpec)

var strippers = map[string]stripper{
	"SandboxSpec.Env": func(p LocalPlan, s *runner.SandboxSpec) {
		s.Env = maps.Clone(s.Env)
		for _, key := range p.OrgConfigKeys {
			delete(s.Env, key)
		}
	},
	// Every interception entry needs an own admitted injection; an org-only
	// interception entry must not silently retain authority after credentials strip.
	"ProxyConfig.MITMHosts": func(_ LocalPlan, s *runner.SandboxSpec) {
		s.ProxyConfig.MITMHosts = slices.DeleteFunc(slices.Clone(s.ProxyConfig.MITMHosts), func(host string) bool {
			return !slices.ContainsFunc(s.ProxyConfig.Injection, func(in runner.InjectionGrant) bool { return hostrules.HostOf(host) == in.Rule.Host })
		})
	},
	// Every grant left in the policy was proven own and OwnerOnly by the
	// credential classification that ran first; nothing remains to drop.
	"ProxyConfig.Policy.EligibleGrants": func(LocalPlan, *runner.SandboxSpec) {},
	"ProxyConfig.Policy.LLMInspection.WorkspaceSecretValues": func(_ LocalPlan, s *runner.SandboxSpec) {
		if li := s.ProxyConfig.Policy.LLMInspection; li != nil {
			li.WorkspaceSecretValues = nil
		}
	},
	"ProxyConfig.Policy.LLMInspection.WorkspaceSecretNames": func(_ LocalPlan, s *runner.SandboxSpec) {
		if li := s.ProxyConfig.Policy.LLMInspection; li != nil {
			li.WorkspaceSecretNames = nil
		}
	},
}

// gates check a RuleRefuse or RuleBound field against the runner, keyed
// "Struct.Field". A gated field with no gate refuses.
var gates = map[string]func(LocalPlan) *Refusal{
	"SandboxSpec.Mounts": func(p LocalPlan) *Refusal {
		for i, m := range p.Spec.Mounts {
			path := indexed("SandboxSpec.Mounts", i)
			if !m.MemberAuthored {
				return refuse(ReasonPlacementCapability, path, "operator host mounts cannot be sent to a local runner")
			}
			if !p.VerifiedLocalPaths[path] {
				return refuse(ReasonPlacementLocalPath, path, "local path is not bound to this runner's allowed roots")
			}
		}
		return nil
	},
	"SandboxSpec.Drive": func(p LocalPlan) *Refusal {
		if d := p.Spec.Drive; d != nil {
			if d.Backend != types.DriveBackendHostPath {
				return refuse(ReasonPlacementCapability, "SandboxSpec.Drive", "organisation storage cannot be sent to a local runner")
			}
			if !p.VerifiedLocalPaths["SandboxSpec.Drive"] {
				return refuse(ReasonPlacementLocalPath, "SandboxSpec.Drive", "drive path is not bound to this runner's allowed roots")
			}
		}
		return nil
	},
}

// CredentialPaths enumerates every present value of a field the table classifies
// as credential-bearing, plus grant eligibility (approval-gated grants are
// absent from Injection). Sorted keys make refusals stable.
func CredentialPaths(s runner.SandboxSpec) []string {
	return credentialPaths(Entries, s)
}

func credentialPaths(entries entrySource, s runner.SandboxSpec) []string {
	out := structPaths(entries, StructSandboxSpec, "SandboxSpec", reflect.ValueOf(s))
	out = append(out, structPaths(entries, StructProxyConfig, "ProxyConfig", reflect.ValueOf(s.ProxyConfig))...)
	for i := range s.ProxyConfig.Policy.EligibleGrants {
		out = append(out, indexed("ProxyConfig.Policy.EligibleGrants", i))
	}
	return out
}

func structPaths(entries entrySource, structName, prefix string, v reflect.Value) []string {
	var out []string
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if !f.IsExported() || !rulesOf(entries(structName, f.Name)).credential {
			continue
		}
		out = append(out, elementPaths(prefix+"."+f.Name, v.Field(i), func(e reflect.Value) bool {
			return nonCredentialElement(structName, f.Name, e)
		})...)
	}
	return out
}

// nonCredentialElement is the one element-level exception: managed settings
// files are platform-authored and allowed; only an AgentOwned file is a secret.
func nonCredentialElement(structName, field string, e reflect.Value) bool {
	return structName == StructSandboxSpec && field == "ManagedFiles" && !e.FieldByName("AgentOwned").Bool()
}

func elementPaths(path string, v reflect.Value, skip func(reflect.Value) bool) []string {
	var out []string
	switch v.Kind() {
	case reflect.Map:
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return cmpString(fmt.Sprint(a.Interface()), fmt.Sprint(b.Interface())) })
		for _, k := range keys {
			if !skip(v.MapIndex(k)) {
				out = append(out, path+"["+fmt.Sprint(k.Interface())+"]")
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if !skip(v.Index(i)) {
				out = append(out, path+"["+strconv.Itoa(i)+"]")
			}
		}
	default:
		if !v.IsZero() {
			out = append(out, path)
		}
	}
	return out
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// enforceTable applies every non-credential row to a copy of the spec and
// returns it: gated fields (refuse or bound) need a registered gate, not_sent
// fields are zeroed unless they are never marshalled, and strip rows run their
// stripper. A rule with nothing implementing it refuses; it is never skipped.
func enforceTable(entries entrySource, p LocalPlan) (runner.SandboxSpec, *Refusal) {
	s := p.Spec
	s.ProxyConfig.Policy = s.ProxyConfig.Policy.Clone()
	if r := enforceStruct(entries, p, &s, StructSandboxSpec, "SandboxSpec", reflect.ValueOf(&s).Elem()); r != nil {
		return runner.SandboxSpec{}, r
	}
	if r := enforceStruct(entries, p, &s, StructProxyConfig, "ProxyConfig", reflect.ValueOf(&s.ProxyConfig).Elem()); r != nil {
		return runner.SandboxSpec{}, r
	}
	return s, nil
}

func enforceStruct(entries entrySource, p LocalPlan, s *runner.SandboxSpec, structName, prefix string, v reflect.Value) *Refusal {
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		rows := entries(structName, f.Name)
		rs, key, field := rulesOf(rows), structName+"."+f.Name, v.Field(i)
		if field.IsZero() && !slices.ContainsFunc(rows, func(e Entry) bool { return e.Path != "" }) {
			continue
		}
		if rs.gated {
			gate, ok := gates[key]
			if !ok {
				return refuse(ReasonPlacementCapability, prefix+"."+f.Name, "no local enforcement is implemented for this field")
			}
			if r := gate(p); r != nil {
				return r
			}
		}
		if rs.notSent && f.Tag.Get("json") != "-" {
			field.SetZero()
		}
		if r := applyStrips(rows, key, prefix+"."+f.Name, p, s); r != nil {
			return r
		}
	}
	return nil
}

func applyStrips(rows []Entry, key, path string, p LocalPlan, s *runner.SandboxSpec) *Refusal {
	for _, e := range rows {
		if e.Rule != RuleStrip || e.Class == ClassComposite {
			continue
		}
		k := key
		if e.Path != "" {
			k += "." + e.Path
		}
		fn, ok := strippers[k]
		if !ok {
			return refuse(ReasonPlacementCredential, path, "no local strip is implemented for this field")
		}
		fn(p, s)
	}
	return nil
}
