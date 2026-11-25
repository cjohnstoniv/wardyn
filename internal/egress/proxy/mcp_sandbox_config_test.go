// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// mcpSandboxRefusedFields are the Config fields an MCP sandbox's proxy may not carry. With
// mcpSandboxConfigFields it is a closed list: a field added to Config fails
// TestMCPSandboxConfigAllowlist until it is put in one of the two.
var mcpSandboxRefusedFields = strings.Fields("SigninListen AllowedPeers SelfLapse DecisionSink GitGrants GitPushWithheld PATGrants " +
	"BrokeredPATGrantIDs ResidentMints ADOGrant AzureGates GitHubAPI AzureARM AWSSigV4 KubeAPI MITMLLM UpstreamProxyURL " +
	"UpstreamProxyNoProxy InternalHosts LLMUpstreams LLMChannelHosts LLMHeaders LLMUnavailableDetail LocalModels " +
	"AllowedModels BedrockRoutes GatewayIdentity RunnerLocalModels Unattended CanonicalPushRepos PushHoldRemovals VM " +
	"PortRule Attribution PushRuleSets PushRuleBase ForgeHosts ActivityIgnoreHosts HarnessToolRules HarnessCredentials " +
	"ViaOrgRoutes MCPServers MCPBindings MCPApproveEach DisableModelUsage NoNetwork")

const mcpRelayDigest = "1f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a7988"

func mcpSandboxConfig() *Config {
	return &Config{
		RunID: uuid.New(), ControlPlaneURL: "http://127.0.0.1:8443", RunToken: "t", ControlPlaneAudience: MCPSandboxAudience,
		MCPRelayInbound: &MCPRelayInboundConfig{Listen: "10.77.0.5:3130", TokenSHA256: mcpRelayDigest, Upstream: "10.88.0.3:8765"},
	}
}

// TestMCPSandboxConfigAllowlist walks Config in the closed-world style of the placement registry:
// every field is either allowed on an MCP sandbox's proxy or refused when set, by its key.
func TestMCPSandboxConfigAllowlist(t *testing.T) {
	ct := reflect.TypeFor[Config]()
	for i := range ct.NumField() {
		f := ct.Field(i)
		allowed, refused := slices.Contains(mcpSandboxConfigFields, f.Name), slices.Contains(mcpSandboxRefusedFields, f.Name)
		if allowed == refused {
			t.Errorf("Config.%s is in %d of the two lists; classify it as allowed or refused on an MCP sandbox's proxy", f.Name, map[bool]int{true: 2, false: 0}[allowed])
			continue
		}
		c := mcpSandboxConfig()
		if f.Name == "ControlPlaneAudience" || f.Name == "MCPRelayInbound" {
			continue // set by the base config, and checked by TestMCPSandboxConfigAudience
		}
		reflect.ValueOf(c).Elem().Field(i).Set(nonZeroOf(f.Type))
		err := c.validateMCPSandboxConfig()
		if allowed && err != nil {
			t.Errorf("Config.%s set: refused (%v), want it allowed", f.Name, err)
		}
		if refused && (err == nil || !strings.Contains(err.Error(), jsonKey(f))) {
			t.Errorf("Config.%s set: err %v, want it refused by its key %q", f.Name, err, jsonKey(f))
		}
	}
	for _, name := range slices.Concat(mcpSandboxConfigFields, mcpSandboxRefusedFields) {
		if _, ok := ct.FieldByName(name); !ok {
			t.Errorf("%s is listed but Config has no such field", name)
		}
	}
}

// nonZeroOf is a value of t that is not its zero value.
func nonZeroOf(t reflect.Type) reflect.Value {
	v := reflect.New(t).Elem()
	switch t.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.String:
		v.SetString("x")
	case reflect.Int, reflect.Int64:
		v.SetInt(1)
	case reflect.Pointer:
		v.Set(reflect.New(t.Elem()))
	case reflect.Map:
		v.Set(reflect.MakeMap(t))
		v.SetMapIndex(reflect.New(t.Key()).Elem(), reflect.New(t.Elem()).Elem())
	case reflect.Slice:
		v.Set(reflect.MakeSlice(t, 1, 1))
	case reflect.Uint8:
		v.SetUint(1)
	case reflect.Array:
		v.Index(0).Set(nonZeroOf(t.Elem()))
	case reflect.Struct:
		v.Field(0).Set(nonZeroOf(t.Field(0).Type))
	default:
		panic(fmt.Sprintf("nonZeroOf: no value for kind %s", t.Kind()))
	}
	return v
}

// TestMCPSandboxConfigAudience: the audience is "" or the MCP sandbox's, refused otherwise at load;
// the relay listener comes with that audience and only with it, and is checked.
func TestMCPSandboxConfigAudience(t *testing.T) {
	base := func(extra string) string {
		return fmt.Sprintf(`{"run_id":%q,"control_plane_url":"http://127.0.0.1:8443","run_token":"t"%s}`, uuid.New(), extra)
	}
	inbound := func(listen, digest, upstream string) string {
		return fmt.Sprintf(`,"control_plane_audience":"wardyn-mcp-sandbox","mcp_relay_inbound":{"listen":%q,"token_sha256":%q,"upstream":%q}`, listen, digest, upstream)
	}
	for _, c := range []struct {
		name, extra, wantErr string
	}{
		{"a run's own proxy", ``, ""},
		{"an MCP sandbox's proxy", inbound("10.77.0.5:3130", mcpRelayDigest, "10.88.0.3:8765"), ""},
		{"upstream outside RFC 1918", inbound("10.77.0.5:3130", mcpRelayDigest, "100.64.0.9:8765"), ""},
		{"with its scrub list", inbound("10.77.0.5:3130", mcpRelayDigest, "10.88.0.3:8765") + `,"relay_scrub_values":["s3cret-value"]`, ""},
		{"listen host left empty", inbound(":3130", mcpRelayDigest, "10.88.0.3:8765"), "mcp_relay_inbound.listen"},
		{"listen unspecified", inbound("0.0.0.0:3130", mcpRelayDigest, "10.88.0.3:8765"), "mcp_relay_inbound.listen"},
		{"listen mapped unspecified", inbound("[::ffff:0.0.0.0]:3130", mcpRelayDigest, "10.88.0.3:8765"), "mcp_relay_inbound.listen"},
		{"scrub list on a run's own proxy", `,"relay_scrub_values":["s3cret-value"]`, "relay_scrub_values are set only"},
		{"another audience", `,"control_plane_audience":"wardyn-internal"`, "control_plane_audience"},
		{"relay listener on a run's own proxy", `,"mcp_relay_inbound":{"listen":":3130","token_sha256":"` + mcpRelayDigest + `","upstream":"10.88.0.3:8765"}`, "mcp_relay_inbound and relay_scrub_values are set only"},
		{"no relay listener", `,"control_plane_audience":"wardyn-mcp-sandbox"`, "needs mcp_relay_inbound"},
		{"a refused field", inbound("10.77.0.5:3130", mcpRelayDigest, "10.88.0.3:8765") + `,"upstream_proxy_url":"http://corp:3128"`, "upstream_proxy_url"},
		{"an MCP server", inbound("10.77.0.5:3130", mcpRelayDigest, "10.88.0.3:8765") + `,"mcp_servers":[{"key":"docs","url":"https://mcp.docs.test/mcp","ordinal":0}]`, "mcp_servers"},
		{"listen host name", inbound("relay:3130", mcpRelayDigest, "10.88.0.3:8765"), "mcp_relay_inbound.listen"},
		{"listen no port", inbound("10.77.0.5", mcpRelayDigest, "10.88.0.3:8765"), "mcp_relay_inbound.listen"},
		{"digest upper-case", inbound("10.77.0.5:3130", strings.ToUpper(mcpRelayDigest), "10.88.0.3:8765"), "token_sha256"},
		{"digest short", inbound("10.77.0.5:3130", mcpRelayDigest[:62], "10.88.0.3:8765"), "token_sha256"},
		{"upstream host name", inbound("10.77.0.5:3130", mcpRelayDigest, "mcp-host:8765"), "mcp_relay_inbound.upstream"},
		{"upstream unspecified", inbound("10.77.0.5:3130", mcpRelayDigest, "0.0.0.0:8765"), "mcp_relay_inbound.upstream"},
		{"upstream loopback", inbound("10.77.0.5:3130", mcpRelayDigest, "127.0.0.1:8765"), "mcp_relay_inbound.upstream"},
		{"upstream metadata address", inbound("10.77.0.5:3130", mcpRelayDigest, "169.254.169.254:80"), "mcp_relay_inbound.upstream"},
	} {
		cfg, err := LoadConfigBytes([]byte(base(c.extra)))
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err %v, want it to name %q", c.name, err, c.wantErr)
		case c.wantErr == "" && cfg.mcpSandbox() != strings.Contains(c.extra, MCPSandboxAudience):
			t.Errorf("%s: mcpSandbox() = %v", c.name, cfg.mcpSandbox())
		}
	}
}
