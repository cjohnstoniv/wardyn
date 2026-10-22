// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The census guard for the proxy-settings gate. A 0.9.x runner rebuilds the
// proxy config from the fields it knows and drops the rest, so a ProxyConfig
// field added since v0.9.8 either names a ProxySetting* (the org then refuses a
// create that sets it on a runner that does not advertise it) or says why a
// runner dropping it fails closed. A new field in neither place fails here.

// proxyConfigFieldsAtV098 is runner.ProxyConfig's field names at tag v0.9.8
// (`git show v0.9.8:internal/runner/runner.go`): every runner older than the
// gate carries these through. Frozen; never add to it.
var proxyConfigFieldsAtV098 = []string{
	"RunToken", "ControlPlaneURL", "ControlPlaneCAPEM", "Policy", "Injection", "MITMCACertPEM", "MITMCAKeyPEM",
	"MITMHosts", "MITMLLM", "GitGrants", "PATGrants", "ADOGrant", "AzureGates", "GitHubAPI", "AzureARM",
	"BrokeredPATGrantIDs", "ResidentMints", "UpstreamProxyURL", "TrustedCAPEM", "InternalHosts",
	"UpstreamProxyNoProxy", "LLMUpstreams", "LLMChannelHosts", "LLMUnavailableDetail", "Unattended",
	"Attribution", "PushRuleSets", "PushRuleBase", "HarnessToolRules", "HarnessCredentials", "ViaOrgRoutes",
	"MCPServers", "MCPBindings",
}

// proxySettingOfField maps each post-v0.9.8 field whose drop by an older runner
// would fail open to the ProxySetting* that gates it.
var proxySettingOfField = map[string]string{
	"AWSSigV4":       ProxySettingAWSSigV4,
	"ScrubResponses": ProxySettingScrubResponses,
	"VM":             ProxySettingVM,
	"PortRule":       ProxySettingPortRule,
	"LocalModels":    ProxySettingLocalModels,
	"KubeAPI":        ProxySettingKubeAPI,
	// Dropped, a held github.com push keys on git's spelling again
	// (proxy.patPushTarget), so an admin's denial of Owner/Repo.git is asked
	// again as owner/repo: the one decision the operator turned it on for is lost.
	"CanonicalPushRepos": ProxySettingCanonicalRepos,
}

// proxyFieldExemptions maps each post-v0.9.8 field left ungated to why a runner
// dropping it fails closed.
var proxyFieldExemptions = map[string]string{
	"DesktopSignins": "the org sets it only when signinTarget holds, which is false for a run with a runner or a pool " +
		"(internal/api run_desktop_signin.go signinTarget; remote.Substrate is no runner.SigninExecer either), so a " +
		"wardyn-runnerd never receives it set; dropped, " +
		"BuildProxyConfig writes no signin_listen (internal/runner/sandbox.go) and the proxy opens no sign-in lane " +
		"(internal/egress/proxy server.go ListenAndServe): the run gets no sign-ins, its one egress lane unchanged",
}

// proxyFieldCensus names each field of t newer than v0.9.8 that is in neither
// map or in both, and each map entry that names no field of t.
func proxyFieldCensus(t reflect.Type, mapped, exempt map[string]string) []string {
	var out []string
	fields := map[string]bool{}
	for f := range t.Fields() {
		fields[f.Name] = true
		if slices.Contains(proxyConfigFieldsAtV098, f.Name) {
			continue
		}
		_, m := mapped[f.Name]
		_, e := exempt[f.Name]
		switch {
		case !m && !e:
			out = append(out, fmt.Sprintf("%s.%s is newer than v0.9.8 and in neither proxySettingOfField (gate it with a ProxySetting*) nor proxyFieldExemptions (say why a runner dropping it fails closed)", t.Name(), f.Name))
		case m && e:
			out = append(out, fmt.Sprintf("%s.%s is in both proxySettingOfField and proxyFieldExemptions", t.Name(), f.Name))
		}
	}
	for mapName, m := range map[string]map[string]string{"proxySettingOfField": mapped, "proxyFieldExemptions": exempt} {
		for name := range m {
			if !fields[name] || slices.Contains(proxyConfigFieldsAtV098, name) {
				out = append(out, fmt.Sprintf("%s names %s, which is not a post-v0.9.8 field of %s", mapName, name, t.Name()))
			}
		}
	}
	slices.Sort(out)
	return out
}

func TestProxyConfigFieldsNewerThanV098AreGatedOrExempt(t *testing.T) {
	if got := proxyFieldCensus(reflect.TypeFor[runner.ProxyConfig](), proxySettingOfField, proxyFieldExemptions); len(got) != 0 {
		t.Fatalf("proxy settings census:\n%s", strings.Join(got, "\n"))
	}
	for name, why := range proxyFieldExemptions {
		if strings.TrimSpace(why) == "" {
			t.Errorf("proxyFieldExemptions[%s] gives no reason", name)
		}
	}
	// The mapped settings are exactly the ones ProxySettingsSet reports, and
	// setting a mapped field alone reports its setting alone.
	var settings []string
	for field, setting := range proxySettingOfField {
		settings = append(settings, setting)
		var pc runner.ProxyConfig
		v := reflect.ValueOf(&pc).Elem().FieldByName(field)
		v.Set(nonZero(v.Type()))
		if got := ProxySettingsSet(pc); !slices.Equal(got, []string{setting}) {
			t.Errorf("setting %s alone reports %v, want [%s]", field, got, setting)
		}
	}
	slices.Sort(settings)
	if want := slices.Sorted(slices.Values(ProxySettings)); !slices.Equal(settings, want) {
		t.Errorf("proxySettingOfField settings %v, ProxySettings %v", settings, want)
	}
}

// nonZero is a set value of type t: true, "on", one element, or a new pointee.
func nonZero(t reflect.Type) reflect.Value {
	v := reflect.New(t).Elem()
	switch t.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.String:
		v.SetString("on")
	case reflect.Pointer:
		v.Set(reflect.New(t.Elem()))
	case reflect.Map:
		v.Set(reflect.MakeMap(t))
		v.SetMapIndex(reflect.New(t.Key()).Elem(), reflect.New(t.Elem()).Elem())
	case reflect.Slice:
		v.Set(reflect.MakeSlice(t, 1, 1))
	default:
		panic("nonZero: unhandled kind " + t.Kind().String())
	}
	return v
}

// TestProxySettingsCensusFiresOnAPlantedField shows the census is not satisfied
// by being silent: a ProxyConfig twin with one extra field is flagged, by name
// and naming both maps; a field in both maps and a stale entry are flagged too.
func TestProxySettingsCensusFiresOnAPlantedField(t *testing.T) {
	base := reflect.TypeFor[runner.ProxyConfig]()
	fields := slices.Collect(base.Fields())
	planted := reflect.StructOf(append(fields, reflect.StructField{Name: "PlantedDoor", Type: reflect.TypeFor[*int]()}))

	got := proxyFieldCensus(planted, proxySettingOfField, proxyFieldExemptions)
	if len(got) != 1 || !strings.Contains(got[0], "PlantedDoor") || !strings.Contains(got[0], "proxySettingOfField") || !strings.Contains(got[0], "proxyFieldExemptions") {
		t.Fatalf("planted field: census = %q", got)
	}
	both := maps.Clone(proxyFieldExemptions)
	both["KubeAPI"] = "planted"
	if got := proxyFieldCensus(base, proxySettingOfField, both); len(got) != 1 || !strings.Contains(got[0], "KubeAPI is in both") {
		t.Fatalf("doubly classified field: census = %q", got)
	}
	stale := maps.Clone(proxyFieldExemptions)
	stale["RunToken"], stale["Gone"] = "planted", "planted"
	if got := proxyFieldCensus(base, proxySettingOfField, stale); len(got) != 2 {
		t.Fatalf("stale exemptions: census = %q", got)
	}
}
