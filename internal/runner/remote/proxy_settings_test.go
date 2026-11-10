// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// proxySettingCases sets each run proxy setting on its own.
var proxySettingCases = map[string]func(*runner.ProxyConfig){
	runnerwire.ProxySettingAWSSigV4:       func(pc *runner.ProxyConfig) { pc.AWSSigV4 = &proxy.AWSSigV4Config{} },
	runnerwire.ProxySettingScrubResponses: func(pc *runner.ProxyConfig) { pc.ScrubResponses = true },
	runnerwire.ProxySettingVM:             func(pc *runner.ProxyConfig) { pc.VM = true },
	runnerwire.ProxySettingPortRule:       func(pc *runner.ProxyConfig) { pc.PortRule = "on" },
	runnerwire.ProxySettingLocalModels: func(pc *runner.ProxyConfig) {
		pc.LocalModels = map[string]types.LocalModelSettings{"http://models.internal:11434": {}}
	},
	runnerwire.ProxySettingKubeAPI:        func(pc *runner.ProxyConfig) { pc.KubeAPI = &proxy.KubeAPIGrant{} },
	runnerwire.ProxySettingCanonicalRepos: func(pc *runner.ProxyConfig) { pc.CanonicalPushRepos = true },
	runnerwire.ProxySettingGatewayIdentity: func(pc *runner.ProxyConfig) {
		pc.GatewayIdentity = []proxy.GatewayIdentityRoute{{Host: "gw.corp.example", Port: 443, ProviderID: "p", Audience: "a"}}
	},
	runnerwire.ProxySettingGitPushWithheld: func(pc *runner.ProxyConfig) {
		pc.GitPushWithheld = map[string]string{"github.com": "this run may not push"}
	},
	runnerwire.ProxySettingForgeHosts:     func(pc *runner.ProxyConfig) { pc.ForgeHosts = []string{"ghe.corp.example"} },
	runnerwire.ProxySettingAllowedModels:  func(pc *runner.ProxyConfig) { pc.ModelLimit = &runner.ModelLimit{Models: []string{"model-a"}} },
	runnerwire.ProxySettingMCPApproveEach: func(pc *runner.ProxyConfig) { pc.MCPApproveEach = &proxy.MCPApproveEachConfig{} },
	runnerwire.ProxySettingNoNetwork:      func(pc *runner.ProxyConfig) { pc.NoNetwork = true },
	runnerwire.ProxySettingRefuseIPLiterals: func(pc *runner.ProxyConfig) {
		pc.Policy.RefuseIPLiterals = true
	},
	runnerwire.ProxySettingLLMHeaders: func(pc *runner.ProxyConfig) {
		pc.LLMHeaders = map[string]map[string]string{"gw.corp.example": {"X-Team": "platform"}}
	},
	runnerwire.ProxySettingAWSSigV4Gateways: func(pc *runner.ProxyConfig) {
		pc.AWSSigV4 = &proxy.AWSSigV4Config{Gateways: []proxy.AWSSigV4Gateway{{Host: "ai-gw.corp.example", Port: 443,
			SigningHost: "bedrock-runtime.us-east-1.amazonaws.com"}}}
	},
}

// proxySettingsOf is what ProxySettingsSet names for the case name sets: that setting alone, but for
// a gateway, which a re-signing grant carries, so its own setting comes first.
func proxySettingsOf(name string) []string {
	if name == runnerwire.ProxySettingAWSSigV4Gateways {
		return []string{runnerwire.ProxySettingAWSSigV4, name}
	}
	return []string{name}
}

// oldCapsJSON is a 0.9.x runner's caps event as it sends it: no proxy_settings key.
const oldCapsJSON = `{"support":{"Classes":["CC1","CC2"]},"cpu_millis_max":4000,"memory_mib_max":8192,"version":"0.9.8","byoi_selftest":true,"retire":true,"resident":true}`

// publishCaps replaces the runner's advertised caps with raw and waits until the substrate holds them.
func publishCaps(t *testing.T, rig *runnertest.Rig, raw string) {
	t.Helper()
	var want runnerwire.Caps
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		t.Fatal(err)
	}
	if err := rig.Server.Peer.Event(ctxT(t), runnerwire.Event{Kind: runnerwire.EventCaps, Data: json.RawMessage(raw)}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if c, ok := rig.Sub.Caps(); ok && c.Version == want.Version {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the replaced caps never arrived")
		}
	}
}

func TestProxySettingsAllNamed(t *testing.T) {
	if len(proxySettingCases) != len(runnerwire.ProxySettings) {
		t.Fatalf("cases %d, settings %d", len(proxySettingCases), len(runnerwire.ProxySettings))
	}
	if got := runnerwire.ProxySettingsSet(runner.ProxyConfig{}); len(got) != 0 {
		t.Fatalf("an unset config names %v", got)
	}
	for name, set := range proxySettingCases {
		var pc runner.ProxyConfig
		set(&pc)
		if got := runnerwire.ProxySettingsSet(pc); !slices.Equal(got, proxySettingsOf(name)) {
			t.Errorf("%s: ProxySettingsSet = %v, want %v", name, got, proxySettingsOf(name))
		}
	}
}

// An older runner's caps event decodes as carrying no proxy setting.
func TestOldRunnerCapsDecodeAsNoProxySettings(t *testing.T) {
	var c runnerwire.Caps
	if err := runnerwire.Unmarshal([]byte(oldCapsJSON), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.ProxySettings) != 0 || !c.Resident {
		t.Fatalf("old caps = %+v", c)
	}
}

func TestCreateSandboxRefusesAProxySettingAnOldRunnerDropsUnread(t *testing.T) {
	for name, set := range proxySettingCases {
		t.Run(name, func(t *testing.T) {
			rig := runnertest.NewRig(t, rid)
			publishCaps(t, rig, oldCapsJSON)
			spec := runner.SandboxSpec{RunID: uuid.New(), Image: "img", ConfinementClass: types.CC1}
			set(&spec.ProxyConfig)
			_, err := rig.Sub.CreateSandbox(ctxT(t), spec)
			var pe *remote.ProxySettingError
			first := proxySettingsOf(name)[0]
			if !errors.As(err, &pe) || pe.Setting != first {
				t.Fatalf("create = %v, want a ProxySettingError naming %s", err, first)
			}
			if msg := err.Error(); !strings.Contains(msg, "wardyn-runnerd is too old for "+first) || !strings.Contains(msg, "upgrade it") {
				t.Fatalf("message = %q", msg)
			}
			if len(rig.Fake.Created) != 0 {
				t.Fatal("the refused create reached the runner")
			}
		})
	}
}

func TestCreateSandboxAdmitsAProxySettingTheRunnerCarries(t *testing.T) {
	for name, set := range proxySettingCases {
		t.Run(name, func(t *testing.T) {
			rig := runnertest.NewRig(t, rid)
			_, spec := create(t, rig, func(s *runner.SandboxSpec) { set(&s.ProxyConfig) })
			if got := runnerwire.ProxySettingsSet(rig.Fake.Created[0].ProxyConfig); !slices.Equal(got, proxySettingsOf(name)) {
				t.Fatalf("the runner received %v for %s (spec %+v)", got, name, spec.ProxyConfig)
			}
		})
	}
}

// With every proxy setting unset, an older runner is admitted and receives byte for byte the
// CreateSandbox payload that was sent before the settings were checked (testdata, encoded by the
// pre-check code).
func TestUnsetProxySettingsOnAnOldRunnerSendTheSameBytes(t *testing.T) {
	golden, err := os.ReadFile("testdata/create_sandbox_unset_proxy_settings.json")
	if err != nil {
		t.Fatal(err)
	}
	spec := runner.SandboxSpec{RunID: uuid.MustParse("6a2c1f0e-0000-4000-8000-000000000001"), Image: "img", ConfinementClass: types.CC1,
		ProxyConfig: runner.ProxyConfig{ControlPlaneURL: "https://org.example", RunToken: "tok", MITMHosts: []string{"api.example"}, Unattended: true}}
	b, err := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: spec})
	if err != nil || string(b) != string(golden) {
		t.Fatalf("the unset payload changed (%v):\n got %s\nwant %s", err, b, golden)
	}
	rig := runnertest.NewRig(t, rid)
	publishCaps(t, rig, oldCapsJSON)
	if _, err := rig.Sub.CreateSandbox(ctxT(t), spec); err != nil {
		t.Fatalf("an unset spec was refused on an old runner: %v", err)
	}
	if got := marshalSent(t, rig, spec.RunID); got != string(golden) {
		t.Fatalf("the runner received a different payload:\n got %s\nwant %s", got, golden)
	}
}

// A laptop local model (#2957) needs a runner that relays it from its own computer. A runner that
// names every proxy setting but local_model_relay (a 0.10 runner from before the relay, or a
// Kubernetes one) is refused before anything is sent, with the named setting; one that names it is
// sent the run. Unflagged, the same spec goes to the older runner as before.
func TestCreateSandboxRefusesALaptopModelOnARunnerWithoutTheRelay(t *testing.T) {
	laptop := func(allow bool) runner.SandboxSpec {
		return runner.SandboxSpec{RunID: uuid.New(), Image: "img", ConfinementClass: types.CC1, ProxyConfig: runner.ProxyConfig{
			LocalModels: map[string]types.LocalModelSettings{"http://host.docker.internal:11434": {Models: []string{"m"}, AllowOnLaptopRunners: allow}}}}
	}
	rig := runnertest.NewRig(t, rid)
	_, err := rig.Sub.CreateSandbox(ctxT(t), laptop(true))
	var pe *remote.ProxySettingError
	if !errors.As(err, &pe) || pe.Setting != runnerwire.ProxySettingLocalModelRelay || !strings.Contains(err.Error(), "too old for local_model_relay") {
		t.Fatalf("create = %v, want a ProxySettingError naming %s", err, runnerwire.ProxySettingLocalModelRelay)
	}
	if len(rig.Fake.Created) != 0 {
		t.Fatal("the refused create reached the runner")
	}
	if _, err := rig.Sub.CreateSandbox(ctxT(t), laptop(false)); err != nil {
		t.Fatalf("an unflagged local model was refused: %v", err)
	}
	named, err := json.Marshal(append(slices.Clone(runnerwire.ProxySettings), runnerwire.ProxySettingLocalModelRelay))
	if err != nil {
		t.Fatal(err)
	}
	publishCaps(t, rig, `{"support":{"Classes":["CC1","CC2"]},"cpu_millis_max":4000,"memory_mib_max":8192,"version":"relay","proxy_settings":`+string(named)+`,"local_model_ports":[11434]}`)
	if _, err := rig.Sub.CreateSandbox(ctxT(t), laptop(true)); err != nil {
		t.Fatalf("a runner naming %s was refused: %v", runnerwire.ProxySettingLocalModelRelay, err)
	}
}

// The person allows a laptop local model's port on their own runner (--local-model-port): a runner that
// relays local models but does not list the port, or lists none, is refused before anything is sent, with
// the port named; one that lists it is sent the run.
func TestCreateSandboxRefusesALaptopModelPortThePersonHasNotAllowed(t *testing.T) {
	spec := func() runner.SandboxSpec {
		return runner.SandboxSpec{RunID: uuid.New(), Image: "img", ConfinementClass: types.CC1, ProxyConfig: runner.ProxyConfig{
			LocalModels: map[string]types.LocalModelSettings{"http://host.docker.internal:11434": {Models: []string{"m"}, AllowOnLaptopRunners: true}}}}
	}
	named, err := json.Marshal(append(slices.Clone(runnerwire.ProxySettings), runnerwire.ProxySettingLocalModelRelay))
	if err != nil {
		t.Fatal(err)
	}
	caps := func(version, ports string) string {
		return `{"support":{"Classes":["CC1","CC2"]},"cpu_millis_max":4000,"memory_mib_max":8192,"version":"` + version + `","proxy_settings":` + string(named) + ports + `}`
	}
	rig := runnertest.NewRig(t, rid)
	for name, ports := range map[string]string{"none set": "", "another port": `,"local_model_ports":[8080]`} {
		publishCaps(t, rig, caps(name, ports))
		_, err := rig.Sub.CreateSandbox(ctxT(t), spec())
		var pe *remote.LocalModelPortError
		if !errors.As(err, &pe) || pe.Port != 11434 || !strings.Contains(err.Error(), "--local-model-port 11434") {
			t.Errorf("%s: create = %v, want a LocalModelPortError naming port 11434", name, err)
		}
	}
	if len(rig.Fake.Created) != 0 {
		t.Fatal("a refused create reached the runner")
	}
	publishCaps(t, rig, caps("allowed", `,"local_model_ports":[8080,11434]`))
	if _, err := rig.Sub.CreateSandbox(ctxT(t), spec()); err != nil {
		t.Fatalf("a runner allowing port 11434 was refused: %v", err)
	}
}

// A run that refuses every push names forge_hosts even with no code host configured or granted
// (the public ones are always on the list), so a runner whose proxy has no deny_all guard, which
// would open github.com as a plain tunnel to a push token kept as a secret, is refused the run
// before anything is sent. The same run without deny_all still goes to that runner.
func TestCreateSandboxRefusesADenyAllRunOnARunnerWithoutTheForgeGuard(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	publishCaps(t, rig, oldCapsJSON)
	spec := func(denyAll bool) runner.SandboxSpec {
		s := runner.SandboxSpec{RunID: uuid.New(), Image: "img", ConfinementClass: types.CC1, ProxyConfig: runner.ProxyConfig{
			MITMHosts: []string{"api.github.com"}}}
		if denyAll {
			s.ProxyConfig.ForgeHosts = types.DenyAllForgeHosts(types.SiteConfig{}, nil)
		}
		return s
	}
	_, err := rig.Sub.CreateSandbox(ctxT(t), spec(true))
	var pe *remote.ProxySettingError
	if !errors.As(err, &pe) || pe.Setting != runnerwire.ProxySettingForgeHosts || !strings.Contains(err.Error(), "wardyn-runnerd is too old for forge_hosts") {
		t.Fatalf("create = %v, want a ProxySettingError naming %s", err, runnerwire.ProxySettingForgeHosts)
	}
	if len(rig.Fake.Created) != 0 {
		t.Fatal("the refused create reached the runner")
	}
	if _, err := rig.Sub.CreateSandbox(ctxT(t), spec(false)); err != nil {
		t.Fatalf("a run without deny_all was refused on an older runner: %v", err)
	}
}

// capsWithout is a caps event naming every proxy setting but setting.
func capsWithout(t *testing.T, setting string) string {
	t.Helper()
	named, err := json.Marshal(slices.DeleteFunc(slices.Clone(runnerwire.ProxySettings), func(s string) bool { return s == setting }))
	if err != nil {
		t.Fatal(err)
	}
	return `{"support":{"Classes":["CC1","CC2"]},"cpu_millis_max":4000,"memory_mib_max":8192,"version":"0.10.0","resident":true,"proxy_settings":` + string(named) + `}`
}

// A runner that does not carry the governed model list is refused a run that has one, at create and at a
// revive's proxy replacement, before anything is sent; the same runner takes a run with no list.
func TestAllowedModelsNeedARunnerThatCarriesThem(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	sb, _ := create(t, rig, nil)
	publishCaps(t, rig, capsWithout(t, runnerwire.ProxySettingAllowedModels))
	limited := runner.SandboxSpec{RunID: uuid.New(), Image: "img", ConfinementClass: types.CC1,
		ProxyConfig: runner.ProxyConfig{ModelLimit: &runner.ModelLimit{Models: []string{"model-a"}}}}
	_, err := rig.Sub.CreateSandbox(ctxT(t), limited)
	var pe *remote.ProxySettingError
	if !errors.As(err, &pe) || pe.Setting != runnerwire.ProxySettingAllowedModels || !strings.Contains(err.Error(), "too old for allowed_models") {
		t.Fatalf("limited create = %v, want a ProxySettingError naming %s", err, runnerwire.ProxySettingAllowedModels)
	}
	if len(rig.Fake.Created) != 1 {
		t.Fatal("the refused create reached the runner")
	}
	unlimited := limited
	unlimited.RunID, unlimited.ProxyConfig = uuid.New(), runner.ProxyConfig{}
	if _, err := rig.Sub.CreateSandbox(ctxT(t), unlimited); err != nil {
		t.Fatalf("a run with no list was refused: %v", err)
	}
	local := strings.TrimPrefix(sb.Ref, "runner:"+rid+"/")
	err = rig.Sub.ReplaceProxy(ctxT(t), sb.Ref, []byte(`{"allowed_models":["model-a"]}`))
	if !errors.As(err, &pe) || pe.Setting != runnerwire.ProxySettingAllowedModels {
		t.Fatalf("limited replace = %v, want a ProxySettingError naming %s", err, runnerwire.ProxySettingAllowedModels)
	}
	if rig.Fake.Called(`replace_proxy {"allowed_models":["model-a"]}`, local) {
		t.Fatal("the refused replace reached the runner")
	}
	if err := rig.Sub.ReplaceProxy(ctxT(t), sb.Ref, []byte(`{"stripped":true}`)); err != nil {
		t.Fatalf("a revive with no list was refused: %v", err)
	}
}

// A runner that signs AWS requests at its proxy but predates gateways (it reports aws_sigv4 and not
// aws_sigv4_gateways) would drop the gateway entry unread, so a run with one is refused there before
// anything is sent; a runner that names it is sent the run with the entry.
func TestCreateSandboxRefusesAGatewayOnARunnerWithoutIt(t *testing.T) {
	withGateway := func(s *runner.SandboxSpec) {
		proxySettingCases[runnerwire.ProxySettingAWSSigV4Gateways](&s.ProxyConfig)
	}
	older, err := json.Marshal(slices.DeleteFunc(slices.Clone(runnerwire.ProxySettings), func(s string) bool {
		return s == runnerwire.ProxySettingAWSSigV4Gateways
	}))
	if err != nil {
		t.Fatal(err)
	}
	rig := runnertest.NewRig(t, rid)
	publishCaps(t, rig, `{"support":{"Classes":["CC1","CC2"]},"cpu_millis_max":4000,"memory_mib_max":8192,"version":"0.10.0-rc.13","proxy_settings":`+string(older)+`}`)
	spec := runner.SandboxSpec{RunID: uuid.New(), Image: "img", ConfinementClass: types.CC1}
	withGateway(&spec)
	_, err = rig.Sub.CreateSandbox(ctxT(t), spec)
	var pe *remote.ProxySettingError
	if !errors.As(err, &pe) || pe.Setting != runnerwire.ProxySettingAWSSigV4Gateways {
		t.Fatalf("create = %v, want a ProxySettingError naming %s", err, runnerwire.ProxySettingAWSSigV4Gateways)
	}
	if len(rig.Fake.Created) != 0 {
		t.Fatal("the refused create reached the runner")
	}
	current := runnertest.NewRig(t, rid)
	create(t, current, withGateway)
	if sig := current.Fake.Created[0].ProxyConfig.AWSSigV4; sig == nil || len(sig.Gateways) != 1 {
		t.Fatalf("the runner received aws_sigv4 %+v, want the gateway entry", sig)
	}
}
