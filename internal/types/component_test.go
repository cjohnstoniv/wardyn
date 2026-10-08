// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// An external test package so the host rule is the real one: the proxy
// package imports types, so types cannot name it.
package types_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func header(name, host string) types.ComponentSecret {
	return types.ComponentSecret{SecretName: name, Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: host}}
}

func env(name, v string) types.ComponentSecret {
	return types.ComponentSecret{SecretName: name, Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: v}}
}

func nItems(n int, f func(i int) string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = f(i)
	}
	return out
}

func TestComponentDefinitionValidate_Accepts(t *testing.T) {
	for name, d := range map[string]types.ComponentDefinition{
		"empty": {},
		"hosts only, every allowed spelling": {
			Hosts: []string{"api.example.com", "*.example.org", "api.example.com:8443", "*.example.org:443", "localhost"},
		},
		"header with defaults, env and config": {
			Hosts: []string{"api.example.com"},
			Secrets: []types.ComponentSecret{
				header("stripe-key", "api.example.com"),
				env("stripe-key", "STRIPE_KEY"),
			},
			Config: map[string]string{"STRIPE_REGION": "eu-west-1", "_X": ""},
		},
		"header to a host whose egress entry carries :443, with its own header and format": {
			Hosts: []string{"api.example.com:443"},
			Secrets: []types.ComponentSecret{{SecretName: "k", Delivery: types.ComponentDelivery{
				Mode: types.ComponentDeliveryHeader, Host: "api.example.com", Header: "X-Api-Key", Format: "%s",
			}}},
		},
		"shared and plain_http on a header (an org row)": {
			Hosts: []string{"api.example.com"},
			Secrets: []types.ComponentSecret{{SecretName: "k", Shared: true, Delivery: types.ComponentDelivery{
				Mode: types.ComponentDeliveryHeader, Host: "api.example.com", PlainHTTP: true,
			}}},
		},
		"at every limit": {
			Hosts: nItems(types.MaxComponentHosts, func(i int) string { return fmt.Sprintf("h%d.example.com", i) }),
			Secrets: func() []types.ComponentSecret {
				var out []types.ComponentSecret
				for i := range types.MaxComponentSecrets {
					out = append(out, env("k", fmt.Sprintf("V%d", i)))
				}
				return out
			}(),
			Config: func() map[string]string {
				out := map[string]string{}
				for i := range types.MaxComponentConfigKeys {
					out[fmt.Sprintf("C%d", i)] = strings.Repeat("x", types.MaxComponentConfigValueBytes)
				}
				return out
			}(),
		},
	} {
		personDefined := !strings.HasSuffix(name, "(an org row)")
		if err := d.Validate(proxy.ValidDomainEntry, personDefined); err != nil {
			t.Errorf("%s: Validate = %v, want nil", name, err)
		}
	}
}

func TestComponentDefinitionValidate_RefusesAndNamesTheField(t *testing.T) {
	hosts := []string{"api.example.com", "*.example.org"}
	with := func(s ...types.ComponentSecret) types.ComponentDefinition {
		return types.ComponentDefinition{Hosts: hosts, Secrets: s}
	}
	delivery := func(d types.ComponentDelivery) types.ComponentDefinition {
		return with(types.ComponentSecret{SecretName: "k", Delivery: d})
	}
	for name, tc := range map[string]struct {
		def  types.ComponentDefinition
		want string
	}{
		"too many hosts": {
			types.ComponentDefinition{Hosts: nItems(types.MaxComponentHosts+1, func(i int) string { return fmt.Sprintf("h%d.example.com", i) })},
			"hosts: 33 entries exceeds the 32-host limit",
		},
		"a URL is not a host":          {types.ComponentDefinition{Hosts: []string{"https://api.example.com/v1"}}, "hosts[0]: "},
		"a mid-label wildcard is dead": {types.ComponentDefinition{Hosts: []string{"api.example.com", "a.*.example.com"}}, "hosts[1]: "},
		"upper case host":              {types.ComponentDefinition{Hosts: []string{"API.example.com"}}, "hosts[0]: write"},
		"host with a trailing dot":     {types.ComponentDefinition{Hosts: []string{"api.example.com."}}, "hosts[0]: write"},
		"host with surrounding space":  {types.ComponentDefinition{Hosts: []string{" api.example.com"}}, "hosts[0]: write"},
		"host listed twice":            {types.ComponentDefinition{Hosts: []string{"api.example.com", "api.example.com"}}, "hosts[1]: "},

		"too many secrets": {
			with(nSecrets(types.MaxComponentSecrets + 1)...), "secrets: 9 entries exceeds the 8-secret limit",
		},
		"secret name is not a secret name": {with(header("Stripe Key", "api.example.com")), "secrets[0].secret_name: "},
		"empty secret name":                {with(header("", "api.example.com")), "secrets[0].secret_name: "},
		"unknown mode":                     {delivery(types.ComponentDelivery{Mode: "proxy_header"}), "secrets[0].delivery.mode: unknown"},
		"no mode":                          {delivery(types.ComponentDelivery{}), "secrets[0].delivery.mode: unknown"},

		"header host not among the hosts":   {with(header("k", "other.example.com")), "secrets[0].delivery.host: "},
		"header without a host":             {with(header("k", "")), "secrets[0].delivery.host: "},
		"header host is the wildcard entry": {with(header("k", "*.example.org")), "secrets[0].delivery.host: "},
		"header host carrying a port":       {with(header("k", "api.example.com:443")), "secrets[0].delivery.host: \"api.example.com:443\" must not carry a port"},
		"header host in another spelling":   {with(header("k", "API.example.com")), "secrets[0].delivery.host: write"},
		"header host covered only by a wildcard entry": {
			with(header("k", "api.example.org")), "secrets[0].delivery.host: \"api.example.org\" must be one of this component's hosts",
		},
		"header name splits the header": {
			delivery(types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "api.example.com", Header: "X-Key\r\nX-Evil"}),
			"secrets[0].delivery.header: ",
		},
		"format with no %s": {
			delivery(types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "api.example.com", Format: "Bearer"}),
			"secrets[0].delivery.format: ",
		},
		"format with two %s": {
			delivery(types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "api.example.com", Format: "%s %s"}),
			"secrets[0].delivery.format: ",
		},
		"header carrying a var": {
			delivery(types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "api.example.com", Var: "X"}),
			"secrets[0].delivery: var and file",
		},
		"two headers for one host": {
			with(header("a", "api.example.com"), env("a", "A"), header("b", "api.example.com")),
			"secrets[2].delivery: delivers to the same place as secrets[0]",
		},

		"env var in lower case":         {with(env("k", "stripe_key")), "secrets[0].delivery.var: "},
		"env var empty":                 {with(env("k", "")), "secrets[0].delivery.var: "},
		"env var starting with a digit": {with(env("k", "1KEY")), "secrets[0].delivery.var: "},
		"env var in the sandbox's own namespace": {
			with(env("k", "WARDYN_TASK_MODE")), "secrets[0].delivery.var: \"WARDYN_TASK_MODE\" is reserved",
		},
		"env carrying a host": {
			delivery(types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: "K", Host: "api.example.com"}),
			"secrets[0].delivery: only var",
		},
		"env carrying plain_http": {
			delivery(types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: "K", PlainHTTP: true}),
			"secrets[0].delivery: only var",
		},
		"two secrets into one variable": {with(env("a", "K"), env("b", "K")), "secrets[1].delivery: delivers to the same place as secrets[0]"},
		"env var that is also a config key": {
			types.ComponentDefinition{Secrets: []types.ComponentSecret{env("k", "REGION")}, Config: map[string]string{"REGION": "x"}},
			"secrets[0].delivery.var: \"REGION\" is also a config key",
		},

		"shared secret delivered into the sandbox": {
			with(types.ComponentSecret{SecretName: "k", Shared: true, Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: "K"}}),
			"secrets[0].shared: ",
		},
		"shared secret delivered as a file": {
			with(types.ComponentSecret{SecretName: "k", Shared: true, Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryFile, File: "k"}}),
			"secrets[0].shared: ",
		},

		"too many config keys": {
			types.ComponentDefinition{Config: func() map[string]string {
				out := map[string]string{}
				for i := range types.MaxComponentConfigKeys + 1 {
					out[fmt.Sprintf("C%d", i)] = "v"
				}
				return out
			}()},
			"config: 33 keys exceeds the 32-key limit",
		},
		"config key in lower case":  {types.ComponentDefinition{Config: map[string]string{"region": "x"}}, "config: key \"region\""},
		"config key in WARDYN_":     {types.ComponentDefinition{Config: map[string]string{"WARDYN_PROXY": "x"}}, "config: key \"WARDYN_PROXY\" is reserved"},
		"config value too long":     {types.ComponentDefinition{Config: map[string]string{"A": strings.Repeat("x", types.MaxComponentConfigValueBytes+1)}}, "config[A]: value exceeds 4096 bytes"},
		"config value with a break": {types.ComponentDefinition{Config: map[string]string{"A": "one\ntwo"}}, "config[A]: value must be printable"},
		"config value not UTF-8":    {types.ComponentDefinition{Config: map[string]string{"A": "\xff"}}, "config[A]: value must be printable"},
	} {
		// Every refusal here is about shape, so it holds for an org row and a person's alike.
		for _, personDefined := range []bool{false, true} {
			err := tc.def.Validate(proxy.ValidDomainEntry, personDefined)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s (person-defined %v): Validate = %v, want an error containing %q", name, personDefined, err, tc.want)
			}
		}
	}
}

func TestComponentDefinitionValidate_SharedAndPlainHTTPAreOrgOnly(t *testing.T) {
	hosts := []string{"api.example.com"}
	shared := types.ComponentDefinition{Hosts: hosts, Secrets: []types.ComponentSecret{{
		SecretName: "k", Shared: true, Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "api.example.com"},
	}}}
	plain := types.ComponentDefinition{Hosts: hosts, Secrets: []types.ComponentSecret{{
		SecretName: "k", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "api.example.com", PlainHTTP: true},
	}}}
	for name, tc := range map[string]struct {
		def  types.ComponentDefinition
		want string
	}{
		"shared":     {shared, "secrets[0].shared: only an organisation component"},
		"plain_http": {plain, "secrets[0].delivery.plain_http: "},
	} {
		if err := tc.def.Validate(proxy.ValidDomainEntry, false); err != nil {
			t.Errorf("%s on an org row: Validate = %v, want nil", name, err)
		}
		err := tc.def.Validate(proxy.ValidDomainEntry, true)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want an error containing %q", name, err, tc.want)
		}
	}
}

func nSecrets(n int) []types.ComponentSecret {
	out := make([]types.ComponentSecret, n)
	for i := range out {
		out[i] = env("k", fmt.Sprintf("V%d", i))
	}
	return out
}

// File delivery is refused until a build can perform it, so a definition can
// never promise a delivery nothing makes. The sentence is the one a person
// reads; ComponentFileDelivery flipping true turns the same input valid.
func TestComponentDefinitionValidate_FileDeliveryFollowsTheBuild(t *testing.T) {
	d := types.ComponentDefinition{Secrets: []types.ComponentSecret{{
		SecretName: "k", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryFile, File: "service-account.json"},
	}}}
	err := d.Validate(proxy.ValidDomainEntry, true)
	if types.ComponentFileDelivery {
		if err != nil {
			t.Fatalf("Validate = %v, want nil: this build delivers files", err)
		}
		return
	}
	const want = "secrets[0].delivery.mode: file delivery is not available in this release"
	if err == nil || err.Error() != want {
		t.Fatalf("Validate = %v, want %q", err, want)
	}
}

func TestValidComponentName(t *testing.T) {
	for _, ok := range []string{"a", "Stripe (test)", "Zahlungs-API für München", strings.Repeat("é", types.MaxComponentNameRunes)} {
		if err := types.ValidComponentName(ok); err != nil {
			t.Errorf("ValidComponentName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", " ", "x ", " x", "a\tb", "a\nb", "\xff", strings.Repeat("x", types.MaxComponentNameRunes+1)} {
		if err := types.ValidComponentName(bad); err == nil || !strings.HasPrefix(err.Error(), "name: ") {
			t.Errorf("ValidComponentName(%q) = %v, want an error naming the field", bad, err)
		}
	}
}

func TestValidateComponentRefs(t *testing.T) {
	id := uuid.New()
	inline := &types.ComponentDefinition{Hosts: []string{"api.example.com"}}
	ok := []types.ComponentRef{{ID: &id}, {Inline: inline}, {Inline: inline, Name: "Stripe"}, {Inline: &types.ComponentDefinition{}}}
	if err := types.ValidateComponentRefs(ok, proxy.ValidDomainEntry); err != nil {
		t.Fatalf("ValidateComponentRefs = %v, want nil", err)
	}
	if err := types.ValidateComponentRefs(nil, proxy.ValidDomainEntry); err != nil {
		t.Fatalf("no components = %v, want nil", err)
	}
	for name, tc := range map[string]struct {
		refs []types.ComponentRef
		want string
	}{
		"neither id nor inline": {[]types.ComponentRef{{ID: &id}, {}}, "components[1]: give exactly one of id and inline"},
		"both id and inline":    {[]types.ComponentRef{{ID: &id, Inline: inline}}, "components[0]: give exactly one of id and inline"},
		"a label on a stored one": {
			[]types.ComponentRef{{ID: &id, Name: "Stripe"}}, "components[0]: name: labels an inline component only",
		},
		"an unprintable label": {[]types.ComponentRef{{Inline: inline, Name: "a\nb"}}, "components[0]: name: "},
		"an invalid inline definition": {
			[]types.ComponentRef{{Inline: &types.ComponentDefinition{Hosts: []string{"https://x"}}}}, "components[0]: inline.hosts[0]: ",
		},
		"too many": {make([]types.ComponentRef, types.MaxComponentRefs+1), "components: 9 entries exceeds the 8-component limit"},
	} {
		err := types.ValidateComponentRefs(tc.refs, proxy.ValidDomainEntry)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: ValidateComponentRefs = %v, want an error containing %q", name, err, tc.want)
		}
	}
}

func TestSecretNameRE(t *testing.T) {
	for _, ok := range []string{"a", "stripe-key", "a.b_c-9", strings.Repeat("a", 128)} {
		if !types.SecretNameRE.MatchString(ok) {
			t.Errorf("SecretNameRE refuses %q", ok)
		}
	}
	for _, bad := range []string{"", "Stripe", "-a", "a-", "a b", "a/b", "ａ", strings.Repeat("a", 129)} {
		if types.SecretNameRE.MatchString(bad) {
			t.Errorf("SecretNameRE accepts %q", bad)
		}
	}
}

func TestValidInjectionFormat(t *testing.T) {
	for _, ok := range []string{"", "%s", "Bearer %s", "token=%s;v=1"} {
		if err := types.ValidInjectionFormat(ok); err != nil {
			t.Errorf("ValidInjectionFormat(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"Bearer", "%s %s", "%d", "%s %d", "100% %s", "Bearer %s\r\nX-Evil: 1"} {
		if err := types.ValidInjectionFormat(bad); err == nil {
			t.Errorf("ValidInjectionFormat(%q) = nil, want an error", bad)
		}
	}
}

// A request or a row that names no component must read exactly as it did
// before the field existed.
func TestComponentWireShape(t *testing.T) {
	if got, _ := json.Marshal(types.ComponentRef{}); string(got) != `{}` {
		t.Errorf("zero ComponentRef = %s, want {}", got)
	}
	d := types.ComponentDefinition{
		Hosts: []string{"api.example.com"},
		Secrets: []types.ComponentSecret{{SecretName: "k", Shared: true, Delivery: types.ComponentDelivery{
			Mode: types.ComponentDeliveryHeader, Host: "api.example.com", Header: "X-Key", Format: "%s", PlainHTTP: true,
		}}, env("k2", "K2")},
		Config: map[string]string{"A": "b"},
	}
	const want = `{"hosts":["api.example.com"],"secrets":[{"secret_name":"k","shared":true,"delivery":` +
		`{"mode":"header","host":"api.example.com","header":"X-Key","format":"%s","plain_http":true}},` +
		`{"secret_name":"k2","delivery":{"mode":"env","var":"K2"}}],"config":{"A":"b"}}`
	got, err := json.Marshal(d)
	if err != nil || string(got) != want {
		t.Fatalf("definition = %s, %v\nwant %s", got, err, want)
	}
}

// A person-defined component names DNS names only: the proxy trusts an exact
// IP in an allowlist as operator-typed and dials it past the private-range
// guard and the corporate upstream. Org rows keep the admin's semantics.
func TestComponentDefinitionValidate_PersonHostsAreDNSNames(t *testing.T) {
	for _, h := range []string{
		"api.example.com", "api.example.com:443", "*.example.com", "*.example.com:8443",
		"localhost", "xn--mnchen-3ya.de", "1password.com", "123.example.com", "0x7f.example.com",
		"a1", "a.b1", "example.0com", "example.c0m",
	} {
		def := types.ComponentDefinition{Hosts: []string{h}}
		for _, personDefined := range []bool{false, true} {
			if err := def.Validate(proxy.ValidDomainEntry, personDefined); err != nil {
				t.Errorf("%q (person-defined %v): Validate = %v, want nil", h, personDefined, err)
			}
		}
	}
	for _, h := range []string{
		// IPv4: dotted, port-qualified, shortened, decimal, hex, octal, mixed.
		"10.0.0.1", "1.2.3.4:443", "127.1", "127.0.1", "2130706433", "2130706433:80",
		"0x7f000001", "0x7f.1", "0x7f.0.0.1", "0X7F.0.0.1", "0177.0.0.1", "017700000001", "0x", "169.254.169.254",
		// IPv6, bare and bracketed with a port; IPv4-mapped.
		"::1", "fd00::1", "[::1]:443", "::ffff:10.0.0.1", "[::ffff:10.0.0.1]:443",
		// A wildcard over an address space, and a name that ends in a number.
		"*.10.0.0", "*.0x7f.1", "internal.10", "host.0x1f",
	} {
		def := types.ComponentDefinition{Hosts: []string{h}}
		err := def.Validate(proxy.ValidDomainEntry, true)
		if err == nil || !strings.Contains(err.Error(), "hosts[0]: ") {
			t.Errorf("%q (person-defined): Validate = %v, want a refusal naming hosts[0]", h, err)
		}
	}
	// Spellings the allowlist grammar itself refuses, whoever writes them.
	for _, h := range []string{"[::1]", "[::ffff:10.0.0.1]", "fe80::1%eth0", "10.0.0.1."} {
		def := types.ComponentDefinition{Hosts: []string{h}}
		for _, personDefined := range []bool{false, true} {
			if err := def.Validate(proxy.ValidDomainEntry, personDefined); err == nil {
				t.Errorf("%q (person-defined %v): Validate = nil, want a refusal", h, personDefined)
			}
		}
	}
	// An org row may name an address, in its canonical spelling.
	for _, h := range []string{"10.0.0.1", "1.2.3.4:443", "fd00::1", "[fd00::1]:8443", "169.254.169.254"} {
		if err := (types.ComponentDefinition{Hosts: []string{h}}).Validate(proxy.ValidDomainEntry, false); err != nil {
			t.Errorf("%q (org row): Validate = %v, want nil", h, err)
		}
	}
	if err := (types.ComponentDefinition{Hosts: []string{"::ffff:10.0.0.1"}}).Validate(proxy.ValidDomainEntry, false); err == nil ||
		!strings.Contains(err.Error(), `as "10.0.0.1"`) {
		t.Errorf("an IPv4-mapped address on an org row: Validate = %v, want its canonical spelling asked for", err)
	}
}

// Ownership comes from the row: Owner "" is an org row, anything else a
// person's; an inline definition on a run is always a person's.
func TestComponentValidate_OwnershipDecidesTheRules(t *testing.T) {
	def := types.ComponentDefinition{Hosts: []string{"10.0.0.1"}}
	if err := (types.Component{Name: "org", Definition: def}).Validate(proxy.ValidDomainEntry); err != nil {
		t.Errorf("org row naming an address: %v, want nil", err)
	}
	err := types.Component{Owner: "alice", Name: "mine", Definition: def}.Validate(proxy.ValidDomainEntry)
	if err == nil || !strings.HasPrefix(err.Error(), "definition.hosts[0]: ") {
		t.Errorf("person row naming an address: %v, want a refusal naming definition.hosts[0]", err)
	}
	if err := (types.Component{Definition: types.ComponentDefinition{}}).Validate(proxy.ValidDomainEntry); err == nil ||
		!strings.HasPrefix(err.Error(), "name: ") {
		t.Errorf("unnamed row: %v, want a refusal naming the field", err)
	}
	ref := []types.ComponentRef{{Inline: &def}}
	if err := types.ValidateComponentRefs(ref, proxy.ValidDomainEntry); err == nil ||
		!strings.Contains(err.Error(), "components[0]: inline.hosts[0]: ") {
		t.Errorf("inline definition naming an address: %v, want a refusal", err)
	}
}

func TestParseDestination(t *testing.T) {
	for in, want := range map[string]types.Destination{
		"api.example.com":       {Host: "api.example.com"},
		"API.Example.com.":      {Host: "api.example.com"},
		" api.example.com ":     {Host: "api.example.com"},
		"api.example.com:443":   {Host: "api.example.com", Port: 443},
		"api.example.com.:8443": {Host: "api.example.com", Port: 8443},
		"*.Example.com":         {Host: "example.com", Wildcard: true},
		"*.example.com:443":     {Host: "example.com", Wildcard: true, Port: 443},
		"10.0.0.1:80":           {Host: "10.0.0.1", Port: 80},
		"[::1]:443":             {Host: "::1", Port: 443},
		"::FFFF:10.0.0.1":       {Host: "10.0.0.1"},
	} {
		got, err := types.ParseDestination(in)
		if err != nil || got != want {
			t.Errorf("ParseDestination(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for in, canon := range map[string]string{
		"API.example.com.:0443": "api.example.com:443",
		"*.example.com:443":     "*.example.com:443",
		"[::1]:443":             "[::1]:443",
		"fd00::1":               "fd00::1",
	} {
		if d, err := types.ParseDestination(in); err != nil || d.String() != canon {
			t.Errorf("ParseDestination(%q).String() = %q, %v; want %q", in, d.String(), err, canon)
		}
	}
	for _, bad := range []string{"", " ", ".", "a.*.example.com", "api.example.com:0", "api.example.com:x", "*.example.com:99999"} {
		if d, err := types.ParseDestination(bad); err == nil {
			t.Errorf("ParseDestination(%q) = %+v, want an error", bad, d)
		}
	}
}

// One comparison for both component-host vetoes, ports ignored: the model
// veto because a model host must not be reached on any port, the collision
// check because the proxy keys an injected credential by bare host.
func TestDestinationOverlapsAtAnyPort(t *testing.T) {
	for _, tc := range []struct {
		a, b    string
		overlap bool
	}{
		{"api.openai.com", "api.openai.com", true},
		{"api.openai.com", "API.OpenAI.com.", true},
		{"api.openai.com", "api.openai.com:443", true},
		{"api.openai.com:8443", "api.openai.com", true},
		{"api.openai.com:8443", "api.openai.com:443", true},
		{"*.openai.com", "api.openai.com", true},
		{"*.openai.com", "API.OpenAI.com.:8443", true},
		{"*.openai.com:443", "api.openai.com:8443", true},
		{"*.openai.com", "*.api.openai.com", true},
		{"*.openai.com", "*.openai.com:443", true},
		{"*.openai.com", "openai.com", false},
		{"*.openai.com", "*.notopenai.com", false},
		{"*.openai.com", "api.notopenai.com", false},
		{"api.openai.com", "api.openai.com.evil.example", false},
		{"api.openai.com", "openai.com", false},
		{"10.0.0.1", "10.0.0.1:443", true},
		{"::ffff:10.0.0.1", "10.0.0.1", true},
	} {
		a, errA := types.ParseDestination(tc.a)
		b, errB := types.ParseDestination(tc.b)
		if errA != nil || errB != nil {
			t.Fatalf("parse %q / %q: %v / %v", tc.a, tc.b, errA, errB)
		}
		// Symmetric by construction; assert it so a later edit cannot make the order matter.
		if a.OverlapsAtAnyPort(b) != tc.overlap || b.OverlapsAtAnyPort(a) != tc.overlap {
			t.Errorf("%q OverlapsAtAnyPort %q = %v/%v, want %v", tc.a, tc.b, a.OverlapsAtAnyPort(b), b.OverlapsAtAnyPort(a), tc.overlap)
		}
	}
}

// A spelling variant of a vetoed host must be refused for what it reaches:
// parsed before Validate, it overlaps the model host, while Validate alone
// would refuse it only for its spelling.
func TestDestinationVetoPrecedesValidate(t *testing.T) {
	model, err := types.ParseDestination("api.openai.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"API.OpenAI.com.", "api.openai.com:8443", "*.openai.com", " api.openai.com "} {
		d, err := types.ParseDestination(h)
		if err != nil || !d.OverlapsAtAnyPort(model) {
			t.Errorf("%q: parsed %+v, %v; want it to overlap the model host", h, d, err)
		}
	}
	err = types.ComponentDefinition{Hosts: []string{"API.OpenAI.com."}}.Validate(proxy.ValidDomainEntry, true)
	if err == nil || !strings.Contains(err.Error(), `write "API.OpenAI.com." as "api.openai.com"`) {
		t.Errorf("Validate alone = %v, want the spelling refusal a veto must come before", err)
	}
}

// A person authors every field below, and the proxy writes the format onto
// every injected request: each is bounded, and a control byte in the format
// is refused here rather than by the transport mid-run.
func TestComponentDefinitionValidate_BoundsPersonAuthoredLengths(t *testing.T) {
	mib := strings.Repeat("A", 1<<20)
	headerWith := func(format string) types.ComponentDefinition {
		return types.ComponentDefinition{Hosts: []string{"api.example.com"}, Secrets: []types.ComponentSecret{{
			SecretName: "k", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "api.example.com", Format: format},
		}}}
	}
	atLimit := func(n int) string { return strings.Repeat("A", n) }
	for name, d := range map[string]types.ComponentDefinition{
		"format at 512 bytes":     headerWith("%s" + strings.Repeat("x", types.MaxComponentHeaderFormatBytes-2)),
		"format with unicode":     headerWith("Clé %s"),
		"env var at 128 bytes":    {Secrets: []types.ComponentSecret{env("k", atLimit(types.MaxComponentEnvNameBytes))}},
		"config key at 128 bytes": {Config: map[string]string{atLimit(types.MaxComponentEnvNameBytes): "v"}},
	} {
		if err := d.Validate(proxy.ValidDomainEntry, true); err != nil {
			t.Errorf("%s: Validate = %v, want nil", name, err)
		}
	}
	for name, tc := range map[string]struct {
		def  types.ComponentDefinition
		want string
	}{
		"format of 513 bytes": {headerWith("%s" + strings.Repeat("x", types.MaxComponentHeaderFormatBytes-1)), "secrets[0].delivery.format: must be printable text of at most 512 bytes"},
		"format of 1 MiB":     {headerWith("%s" + mib), "secrets[0].delivery.format: "},
		"format with NUL":     {headerWith("Bearer %s\x00"), "secrets[0].delivery.format: "},
		"format with a tab":   {headerWith("Bearer\t%s"), "secrets[0].delivery.format: "},
		"format with DEL":     {headerWith("Bearer %s\x7f"), "secrets[0].delivery.format: "},
		"env var of 129 bytes": {
			types.ComponentDefinition{Secrets: []types.ComponentSecret{env("k", atLimit(types.MaxComponentEnvNameBytes+1))}},
			"secrets[0].delivery.var: must be at most 128 bytes",
		},
		"env var of 1 MiB": {types.ComponentDefinition{Secrets: []types.ComponentSecret{env("k", mib)}}, "secrets[0].delivery.var: must be at most 128 bytes"},
		"config key of 129 bytes": {
			types.ComponentDefinition{Config: map[string]string{atLimit(types.MaxComponentEnvNameBytes + 1): "v"}},
			"config: key must be at most 128 bytes",
		},
		"config key of 1 MiB": {types.ComponentDefinition{Config: map[string]string{mib: "v"}}, "config: key must be at most 128 bytes"},
	} {
		for _, personDefined := range []bool{false, true} {
			err := tc.def.Validate(proxy.ValidDomainEntry, personDefined)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s (person-defined %v): Validate = %v, want an error containing %q", name, personDefined, err, tc.want)
			}
		}
	}
}
