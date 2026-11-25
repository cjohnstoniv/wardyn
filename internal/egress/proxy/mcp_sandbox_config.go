// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// An MCP sandbox's proxy: a stock wardyn-proxy whose config names the MCP
// sandbox audience. It stands between one local MCP server, in a sandbox of its
// own, and everything else: the server's egress leaves only through it, under
// the server's own sealed policy, and the agent's proxy reaches the server only
// through its relay listener (mcp_relay_inbound.go). Its config is an
// allowlist, so nothing of the agent's run (a credential broker, a model route,
// a harness binding) can ride into it.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// MCPSandboxAudience is the control-plane audience of an MCP sandbox proxy's
// token. The control plane serves that token on a few routes for one sandbox
// only; the proxy calls the same paths as a run's own proxy.
const MCPSandboxAudience = "wardyn-mcp-sandbox"

// MCPRelayTarget is how a run's own proxy reaches a server in its own sandbox:
// the MCP proxy's relay listener on the run's relay link, and the relay token
// that listener knows by its SHA-256 alone.
type MCPRelayTarget struct {
	// Addr is the relay listener's literal private IP and port, never a host name.
	Addr string `json:"addr"`
	// Token is the relay token, base64url; it is sent only in the relay path.
	Token string `json:"token"`
}

// MCPRelayInboundConfig is an MCP sandbox proxy's relay listener.
type MCPRelayInboundConfig struct {
	// Listen is the relay-link address the listener binds; the runner rewrites
	// its host to the proxy's address on that link.
	Listen string `json:"listen"`
	// TokenSHA256 is the lower-case hex SHA-256 of the relay token.
	TokenSHA256 string `json:"token_sha256"`
	// Upstream is wardyn-mcp-host's literal IP and port on the MCP sandbox's network.
	Upstream string `json:"upstream"`
}

// mcpSandboxConfigFields are the Config fields an MCP sandbox proxy may carry.
// Every other field must be its zero value. A field added to Config later is
// refused here until it is classified (TestMCPSandboxConfigAllowlist).
var mcpSandboxConfigFields = []string{
	"RunID", "ControlPlaneURL", "ControlPlaneCAPEM", "RunToken", "ControlPlaneAudience", "Policy",
	"LiveDenies", "Injection", "Listen", "DecisionBufferSize", "DecisionSpoolDir", "MITMCACertPEM",
	"MITMCAKeyPEM", "MITMHosts", "TrustedCAPEM", "HardDeny", "ScrubResponses", "MCPRelayInbound",
}

// mcpSandbox reports whether this is an MCP sandbox's proxy.
func (c *Config) mcpSandbox() bool { return c.ControlPlaneAudience == MCPSandboxAudience }

// forwardHandler is the forward listener's handler. An MCP sandbox's proxy
// serves none of the local /wardyn/ routes (localRoutesRefused).
func (p *Proxy) forwardHandler(cfg *Config) http.Handler {
	if cfg.mcpSandbox() {
		return p.localRoutesRefused(p.countedHandler())
	}
	return p.countedHandler()
}

// validateMCPSandboxConfig refuses an audience other than "" or
// MCPSandboxAudience, a relay listener on a run's own proxy, and, under the MCP
// sandbox audience, a missing relay listener or any field outside the
// allowlist, by its key.
func (c *Config) validateMCPSandboxConfig() error {
	switch c.ControlPlaneAudience {
	case "":
		if c.MCPRelayInbound != nil {
			return errors.New("config: mcp_relay_inbound is set only with control_plane_audience " + MCPSandboxAudience)
		}
		return nil
	case MCPSandboxAudience:
	default:
		return fmt.Errorf("config: control_plane_audience %q is not one of \"\" and %q", c.ControlPlaneAudience, MCPSandboxAudience)
	}
	v := reflect.ValueOf(c).Elem()
	var refused []string
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if !slices.Contains(mcpSandboxConfigFields, f.Name) && !v.Field(i).IsZero() {
			refused = append(refused, jsonKey(f))
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("config: an MCP sandbox's proxy carries none of %s", strings.Join(refused, ", "))
	}
	if c.MCPRelayInbound == nil {
		return errors.New("config: an MCP sandbox's proxy needs mcp_relay_inbound")
	}
	return c.MCPRelayInbound.validate()
}

// jsonKey is a field's JSON key, its Go name when it has none.
func jsonKey(f reflect.StructField) string {
	if k, _, _ := strings.Cut(f.Tag.Get("json"), ","); k != "" {
		return k
	}
	return f.Name
}

func (in *MCPRelayInboundConfig) validate() error {
	host, port, err := net.SplitHostPort(in.Listen)
	if err != nil || !validPort(port) {
		return fmt.Errorf("config: mcp_relay_inbound.listen %q is not an address and port", in.Listen)
	}
	if a, err := netip.ParseAddr(host); host != "" && (err != nil || a.IsUnspecified()) {
		return fmt.Errorf("config: mcp_relay_inbound.listen %q: the host is a literal address or empty", in.Listen)
	}
	if b, err := hex.DecodeString(in.TokenSHA256); err != nil || len(b) != 32 || strings.ToLower(in.TokenSHA256) != in.TokenSHA256 {
		return errors.New("config: mcp_relay_inbound.token_sha256 is not a lower-case SHA-256 hex digest")
	}
	if ap, err := netip.ParseAddrPort(in.Upstream); err != nil || ap.Addr().IsUnspecified() || ap.Port() == 0 {
		return fmt.Errorf("config: mcp_relay_inbound.upstream %q is not a literal address and port", in.Upstream)
	}
	return nil
}

func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && n <= 65535
}
