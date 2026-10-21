// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"encoding/json"
	"errors"
	"os"
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
		if got := runnerwire.ProxySettingsSet(pc); len(got) != 1 || got[0] != name {
			t.Errorf("%s: ProxySettingsSet = %v", name, got)
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
			if !errors.As(err, &pe) || pe.Setting != name {
				t.Fatalf("create = %v, want a ProxySettingError naming %s", err, name)
			}
			if msg := err.Error(); !strings.Contains(msg, "wardyn-runnerd is too old for "+name) || !strings.Contains(msg, "upgrade it") {
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
			if got := runnerwire.ProxySettingsSet(rig.Fake.Created[0].ProxyConfig); len(got) != 1 || got[0] != name {
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
