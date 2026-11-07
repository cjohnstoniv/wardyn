// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestStrictestToolEffectApproveEach(t *testing.T) {
	const tool = "mcp__jira__delete_issue"
	set := func(h string, e types.ToolEffect) types.HarnessToolRules {
		return types.HarnessToolRules{Harness: h, Rules: []types.ToolRule{{Tool: tool, Effect: e}}}
	}
	for _, c := range []struct {
		name string
		sets []types.HarnessToolRules
		want types.ToolEffect
	}{
		{"approve_each beats allow", []types.HarnessToolRules{set("a", types.ToolApproveEach), set("b", types.ToolAllow)}, types.ToolApproveEach},
		{"approve_each beats hold", []types.HarnessToolRules{set("a", types.ToolHold), set("b", types.ToolApproveEach)}, types.ToolApproveEach},
		{"deny beats approve_each", []types.HarnessToolRules{set("a", types.ToolApproveEach), set("b", types.ToolDeny)}, types.ToolDeny},
		{"a set with no opinion counts as hold", []types.HarnessToolRules{set("a", types.ToolAllow), {Harness: "b"}}, types.ToolHold},
		{"approve_each beats a set with no opinion", []types.HarnessToolRules{set("a", types.ToolApproveEach), {Harness: "b"}}, types.ToolApproveEach},
		{"an unknown effect is not freer than hold", []types.HarnessToolRules{set("a", types.ToolHold), set("b", "maybe")}, "maybe"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := &Proxy{harnessRules: c.sets}
			got, _, ok := p.strictestToolEffect(tool)
			if !ok || got != c.want {
				t.Fatalf("strictestToolEffect = %q (ok %v), want %q", got, ok, c.want)
			}
		})
	}
}

func approveEachBase() map[string]any {
	return map[string]any{
		"run_id":               uuid.New().String(),
		"control_plane_url":    "https://wardynd:8443",
		"control_plane_ca_pem": testCPCAPEM,
		"run_token":            "tok",
	}
}

func withApproveEachRule(m map[string]any) map[string]any {
	m["harness_tool_rules"] = []types.HarnessToolRules{{Harness: "codex-cli", Rules: []types.ToolRule{{Tool: "mcp__jira__delete_issue", Effect: types.ToolApproveEach}}}}
	return m
}

func loadApproveEach(t *testing.T, m map[string]any) error {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadConfigBytes(b)
	return err
}

// TestApproveEachConfigKeyTravelsWithTheRule: the sealed config carries
// mcp_approve_each exactly when a dispatched rule is approve_each. A proxy
// that predates the effect refuses the key, so the run fails at start instead
// of forwarding every call that is not denied; a current proxy refuses a rule
// that arrives without the key, so a writer that forgot it fails closed too.
func TestApproveEachConfigKeyTravelsWithTheRule(t *testing.T) {
	win := map[string]any{"windows": map[string]int{"claude-code": 240, "codex-cli": 45}}

	ruleAndKey := withApproveEachRule(approveEachBase())
	ruleAndKey["mcp_approve_each"] = win
	if err := loadApproveEach(t, ruleAndKey); err != nil {
		t.Fatalf("a config with the rule and the key must load: %v", err)
	}
	runWide := approveEachBase()
	runWide["policy"] = map[string]any{"tool_rules": []types.ToolRule{{Tool: "mcp__jira__delete_issue", Effect: types.ToolApproveEach}}}
	runWide["mcp_approve_each"] = win
	if err := loadApproveEach(t, runWide); err != nil {
		t.Fatalf("a run-wide rule with the key must load: %v", err)
	}
	empty := withApproveEachRule(approveEachBase())
	empty["mcp_approve_each"] = map[string]any{}
	if err := loadApproveEach(t, empty); err != nil {
		t.Fatalf("an empty marker (every harness on the default window) must load: %v", err)
	}

	if err := loadApproveEach(t, withApproveEachRule(approveEachBase())); err == nil || !strings.Contains(err.Error(), "mcp_approve_each") {
		t.Errorf("an approve_each rule with no key must be refused naming it, got %v", err)
	}
	keyOnly := approveEachBase()
	keyOnly["mcp_approve_each"] = win
	if err := loadApproveEach(t, keyOnly); err == nil || !strings.Contains(err.Error(), "mcp_approve_each") {
		t.Errorf("the key with no approve_each rule must be refused naming it, got %v", err)
	}
	for _, w := range []map[string]int{{"codex-cli": 0}, {"codex-cli": -1}, {"codex-cli": 301}, {"": 45}} {
		bad := withApproveEachRule(approveEachBase())
		bad["mcp_approve_each"] = map[string]any{"windows": w}
		if err := loadApproveEach(t, bad); err == nil {
			t.Errorf("windows %v must be refused", w)
		}
	}
}

// TestOlderProxyConfigRefusesApproveEach decodes a current config with the
// previous wire struct (Config without the key, built by reflection so it
// cannot drift) and against the key set of each release that predates the effect.
func TestOlderProxyConfigRefusesApproveEach(t *testing.T) {
	m := withApproveEachRule(approveEachBase())
	m["mcp_approve_each"] = map[string]any{}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	var fields []reflect.StructField
	for f := range reflect.TypeFor[Config]().Fields() {
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name == "mcp_approve_each" {
			continue
		}
		fields = append(fields, f)
	}
	previous := reflect.New(reflect.StructOf(fields))
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(previous.Interface()); err == nil || !strings.Contains(err.Error(), "mcp_approve_each") {
		t.Fatalf("the previous wire struct must refuse the key, got %v", err)
	}
	// The same config without the rule and the key still loads there.
	plain, _ := json.Marshal(approveEachBase())
	dec = json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	if err := dec.Decode(reflect.New(reflect.StructOf(fields)).Interface()); err != nil {
		t.Fatalf("a config without the key must load on the previous struct: %v", err)
	}

	// The releases that predate the effect, by name. Later snapshots roll forward from
	// current.txt and rightly list the key, so they are never checked here.
	for _, name := range []string{"v0.9.3.txt"} {
		b, err := os.ReadFile(configKeysDir + name)
		if err != nil {
			t.Fatal(err)
		}
		if slicesContainsLine(string(b), "mcp_approve_each") {
			t.Errorf("%s lists mcp_approve_each, but that release predates the effect", name)
		}
	}
}

func slicesContainsLine(s, line string) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}
