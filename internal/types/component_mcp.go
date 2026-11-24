// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// The MCP component kind: how a local server starts and how a definition is validated.

// ComponentMCPLaunch is how a local MCP server starts: Command with Args, no
// shell. Command is npx or an absolute path to a program in the image; a
// package npx fetches is pinned to one exact version.
type ComponentMCPLaunch struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// ComponentMCPSandbox is a stdio server's own sandbox: the policy its proxy holds.
type ComponentMCPSandbox struct {
	Policy ComponentMCPSandboxPolicy `json:"policy"`
}

// ComponentMCPSandboxPolicy is how a stdio server's sandbox policy is made. Mode is same (the run's egress,
// the default when the block is absent), narrower (the same, less Overlay) or own (Spec, clamped to the
// organisation's ceiling).
type ComponentMCPSandboxPolicy struct {
	Mode    string          `json:"mode"`
	Overlay *CeilingOverlay `json:"overlay,omitempty"`
	Spec    *RunPolicySpec  `json:"spec,omitempty"`
}

// The sandbox policy modes.
const (
	ComponentMCPSandboxSame     = "same"
	ComponentMCPSandboxNarrower = "narrower"
	ComponentMCPSandboxOwn      = "own"
)

// RemoteMCP reports whether the definition is a remote MCP server, which only
// the broker reaches. A transport other than stdio counts as remote, as every
// server did before local ones existed.
func (d ComponentDefinition) RemoteMCP() bool {
	return d.MCP != nil && d.MCP.Transport != ComponentMCPTransportStdio
}

// LocalMCP reports whether the definition is an MCP server that starts from a
// command, which runs only in a sandbox of its own.
func (d ComponentDefinition) LocalMCP() bool {
	return d.MCP != nil && d.MCP.Transport == ComponentMCPTransportStdio
}

// harnessIDRE is a harness id as tools[].id names one.
var harnessIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// npmPinnedRE is one npm package at one exact version: name@1.4.2 or
// @scope/name@1.4.2-rc.1. A range, a tag such as latest, or no version at
// all would let the package that runs change under the definition.
var npmPinnedRE = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*@[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// validateMCP checks the mcp block. A remote server's credential goes to the
// server and never into the sandbox, so its only secret mode is a header to
// the server's own host, over TLS. A local server's secrets follow every
// component's rules.
func (d ComponentDefinition) validateMCP(personDefined bool) error {
	m := d.MCP
	if m == nil {
		return nil
	}
	if d.CLI != nil {
		return errors.New("mcp: a component is an MCP server or a CLI credential, not both")
	}
	switch m.Transport {
	case ComponentMCPTransportHTTP:
		if m.Sandbox != nil {
			return errors.New("mcp.sandbox: only a server that starts from a command (stdio) runs in a sandbox of its own")
		}
	case ComponentMCPTransportStdio:
		if m.GatewayAudience != "" {
			return errors.New("mcp.gateway_audience: only a remote server is sent the run's identity")
		}
		if err := m.validateLocal(); err != nil {
			return err
		}
		return m.Sandbox.validate()
	case "sse":
		return errors.New("mcp.transport: servers that use SSE are not supported yet; use the server's streamable HTTP address")
	default:
		return fmt.Errorf("mcp.transport: %q is not http or stdio", m.Transport)
	}
	if m.Launch != nil {
		return errors.New("mcp.launch: a remote server is reached at its url; only a stdio server has a launch command")
	}
	host, err := MCPURLDestination(m.URL)
	if err != nil {
		return fmt.Errorf("mcp.url: %w", err)
	}
	if err := m.validateGatewayAudience(personDefined); err != nil {
		return err
	}
	if !slices.ContainsFunc(d.Hosts, func(h string) bool { return mcpHostAdmits(h, host) }) {
		return fmt.Errorf("mcp.url: its host %q must be one of this component's hosts", host.String())
	}
	for i, s := range d.Secrets {
		del := s.Delivery
		switch {
		case del.Mode != ComponentDeliveryHeader:
			return fmt.Errorf("secrets[%d].delivery.mode: an MCP server's secret is sent to the server as a header, never into the sandbox", i)
		case del.Host != host.Host:
			return fmt.Errorf("secrets[%d].delivery.host: an MCP server's secret goes to the server's host %q", i, host.Host)
		case del.PlainHTTP:
			return fmt.Errorf("secrets[%d].delivery.plain_http: an MCP server's secret is only ever sent over TLS", i)
		}
	}
	return nil
}

// validateGatewayAudience checks the audience of a remote server: an organisation's
// alone, one line of at most MaxComponentMCPGatewayAudience bytes.
func (m ComponentMCP) validateGatewayAudience(personDefined bool) error {
	a := m.GatewayAudience
	switch {
	case a == "":
	case personDefined:
		return errors.New("mcp.gateway_audience: only an organisation's server is sent the run's identity")
	case len(a) > MaxComponentMCPGatewayAudience || strings.TrimSpace(a) != a || !printable(a):
		return fmt.Errorf("mcp.gateway_audience: must be printable text of at most %d bytes, with no space at either end", MaxComponentMCPGatewayAudience)
	}
	return nil
}

// validateLocal checks a stdio server: a launch command and no url.
func (m ComponentMCP) validateLocal() error {
	if m.URL != "" {
		return errors.New("mcp.url: a stdio server runs in the sandbox from its launch command and has no url")
	}
	l := m.Launch
	if l == nil {
		return errors.New("mcp.launch: a stdio server needs the command that starts it")
	}
	c := l.Command
	if len(c) > MaxComponentMCPArgBytes || !printable(c) || c != "npx" && (!path.IsAbs(c) || path.Clean(c) != c) {
		return fmt.Errorf("mcp.launch.command: %q must be npx or the absolute path of a program in the image", c)
	}
	if len(l.Args) > MaxComponentMCPArgs {
		return fmt.Errorf("mcp.launch.args: %d entries exceeds the %d-argument limit", len(l.Args), MaxComponentMCPArgs)
	}
	for i, a := range l.Args {
		if len(a) > MaxComponentMCPArgBytes || !printable(a) {
			return fmt.Errorf("mcp.launch.args[%d]: must be printable text of at most %d bytes", i, MaxComponentMCPArgBytes)
		}
	}
	if path.Base(c) != "npx" {
		return nil
	}
	for i, a := range l.Args {
		switch {
		case a == "-y" || a == "--yes":
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("mcp.launch.args[%d]: npx takes only -y before the package here; name the package as its first argument", i)
		case !npmPinnedRE.MatchString(a):
			return fmt.Errorf("mcp.launch.args[%d]: %q must pin one exact version, such as @example/mcp-server@1.4.2", i, a)
		default:
			return nil
		}
	}
	return errors.New("mcp.launch.args: npx needs the package to run, pinned to one exact version, such as @example/mcp-server@1.4.2")
}

// validate checks a stdio server's sandbox block: a mode of the three, an overlay only on narrower and a
// spec only on own, nothing in a spec an MCP sandbox cannot have, and no hold rule, which raises a kind of
// approval an MCP sandbox may not raise.
func (b *ComponentMCPSandbox) validate() error {
	if b == nil {
		return nil
	}
	p := b.Policy
	switch p.Mode {
	case ComponentMCPSandboxSame, ComponentMCPSandboxNarrower, ComponentMCPSandboxOwn:
	default:
		return fmt.Errorf("mcp.sandbox.policy.mode: %q is not same, narrower or own", p.Mode)
	}
	if p.Overlay != nil && p.Mode != ComponentMCPSandboxNarrower {
		return errors.New("mcp.sandbox.policy.overlay: only the narrower mode takes an overlay")
	}
	if p.Spec != nil && p.Mode != ComponentMCPSandboxOwn {
		return errors.New("mcp.sandbox.policy.spec: only the own mode takes a spec")
	}
	if p.Spec != nil {
		if knob := mcpSandboxSpecSetKnob(p.Spec); knob != "" {
			return fmt.Errorf("mcp.sandbox.policy.spec.%s: an MCP sandbox has no harness, terminal, desktop or repository, so this stays empty", knob)
		}
		if err := mcpSandboxNoHold("mcp.sandbox.policy.spec", p.Spec.HostRules); err != nil {
			return err
		}
	}
	if p.Overlay != nil && p.Overlay.HostRules != nil {
		return mcpSandboxNoHold("mcp.sandbox.policy.overlay", *p.Overlay.HostRules)
	}
	return nil
}

// mcpSandboxSpecSetKnob names the first knob of spec that an MCP sandbox's policy always leaves empty and
// spec sets, "" when there is none.
func mcpSandboxSpecSetKnob(spec *RunPolicySpec) string {
	for _, k := range []struct {
		name string
		set  bool
	}{
		{"workspace_mounts", len(spec.WorkspaceMounts) > 0},
		{"workspace_repos", len(spec.WorkspaceRepos) > 0},
		{"llm_inspection", spec.LLMInspection != nil},
		{"ui_apps", len(spec.UIApps) > 0},
		{"tool_rules", len(spec.ToolRules) > 0},
		{"git_push_any_branch", spec.GitPushAnyBranch},
		{"push_rules", spec.PushRules != nil},
		{"azure_devops_capabilities", len(spec.AzureDevOpsCapabilities) > 0},
		{"github_capabilities", len(spec.GitHubCapabilities) > 0},
		{"harness_inner_walls", spec.HarnessInnerWalls != nil},
		{"desktop", spec.Desktop != nil},
		{"recording", spec.Recording != ""},
	} {
		if k.set {
			return k.name
		}
	}
	return ""
}

// mcpSandboxNoHold refuses a host rule whose effect is hold.
func mcpSandboxNoHold(field string, sets []HostRuleSet) error {
	for i, set := range sets {
		for j, r := range set.Rules {
			if r.Effect == HostRuleHold {
				return fmt.Errorf("%s.host_rules[%d].rules[%d].effect: hold is not allowed in an MCP sandbox's policy; use allow or deny", field, i, j)
			}
		}
	}
	return nil
}
