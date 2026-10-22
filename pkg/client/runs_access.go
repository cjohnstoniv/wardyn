// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import "github.com/cjohnstoniv/wardyn/internal/types"

// AccessFacts is what a draft brings in, projected from what the dry-run doors
// decided: POST /runs/policy-preview and POST /runs/preflight answer it from one
// function, so the two cannot disagree about a request both of them admit.
// Every list is an array, never null.
type AccessFacts struct {
	// Rows are the things that bring access, inherited first (harness,
	// repositories, command), then the ones the person added.
	Rows []AccessRow `json:"rows"`
	// Hosts is the Network total: every destination a row or the policy brings.
	Hosts []AccessHost `json:"hosts"`
	// Credentials is the Credentials total.
	Credentials []AccessCredential `json:"credentials"`
	// Unlisted is what happens to a host nothing lists.
	Unlisted AccessUnlisted `json:"unlisted"`
	// Harnesses is the tool policy per included harness.
	Harnesses []ToolPolicyFact `json:"harnesses"`
	// Setup is what the person still has to do before launch.
	Setup []PreflightItem `json:"setup"`
}

// AccessRow is one thing that brings access. Kind is harness, repository,
// command, secret, host or custom (drive, cli, mcp and api are reserved for the
// lanes that add them); From is the row's chip: harness, repository, command or
// added. Status is ready, needs_setup, unavailable or refused. Hosts and
// Credentials are keys into AccessFacts.Hosts and AccessFacts.Credentials.
type AccessRow struct {
	ID          string           `json:"id"`
	Kind        string           `json:"kind"`
	From        string           `json:"from"`
	Name        string           `json:"name"`
	Status      string           `json:"status"`
	Reason      string           `json:"reason,omitempty"`
	Hosts       []string         `json:"hosts"`
	Credentials []string         `json:"credentials"`
	InImage     string           `json:"in_image,omitempty"`
	MCP         *AccessMCP       `json:"mcp,omitempty"`
	Source      ProvenanceSource `json:"source"`
}

// AccessMCP is an MCP server row's binding: the server key in
// mcp__<server>__<tool>, its transport and the harnesses it is turned on for.
type AccessMCP struct {
	ServerKey string   `json:"server_key"`
	Transport string   `json:"transport"`
	Harnesses []string `json:"harnesses"`
}

// AccessHost is one destination as the policy spells it (a port is kept). Via
// is egress, git_broker, model_lane, mcp_broker or injected. Rows are the
// AccessRow ids that bring it, or "policy" when no row does. Verdict is
// allowed, needs_approval or not_allowed; Reason (not_allowed only) is clamped,
// denied_list, capability or always_deny.
type AccessHost struct {
	Host string `json:"host"`
	// Port is the port the host is spelled with, absent when it names none. A spelled port is matched
	// as the proxy matches one: an allow or deny entry for another port does not cover it.
	Port    int              `json:"port,omitempty"`
	Via     string           `json:"via"`
	Rows    []string         `json:"rows"`
	Verdict string           `json:"verdict"`
	Reason  string           `json:"reason,omitempty"`
	Source  ProvenanceSource `json:"source"`
}

// AccessCredential is one credential the run is handed. Kind is
// model_provider, github_app, azure_arm, aws_role, secret, ado_signin, git_pat
// or ssh_key; Owner is own, shared or org_app; Delivery is proxy, git_broker,
// git_helper, sandbox_env, sandbox_file or sandbox_short_lived. LifetimeSec is a
// minted value's ceiling, 0 when the run's lifetime bounds it or Wardyn does not
// set it. AfterEnd is gone, expires or stays.
type AccessCredential struct {
	ID    string `json:"id"`
	Row   string `json:"row"`
	Kind  string `json:"kind"`
	Owner string `json:"owner"`
	// Label names the credential by its kind and whose it is, never by a secret's name. Empty for a
	// model credential, which the console names by its provider.
	Label       string `json:"label,omitempty"`
	Delivery    string `json:"delivery"`
	LifetimeSec int    `json:"lifetime_sec,omitempty"`
	Renewed     bool   `json:"renewed,omitempty"`
	AfterEnd    string `json:"after_end"`
	// Namespaces and Access (read_only or read_write) say what a cluster
	// account (kind kube_api) may reach; absent for every other kind.
	Namespaces []string `json:"namespaces,omitempty"`
	Access     string   `json:"access,omitempty"`
}

// AccessUnlisted is the line for a host nothing lists: the effective
// first-use mode, whether allow-all makes it inert, the hold for
// wait_for_review, and who decides a request (you, admin or you_or_admin).
type AccessUnlisted struct {
	Mode     types.FirstUseMode `json:"mode"`
	AllowAll bool               `json:"allow_all"`
	HoldSec  int                `json:"hold_sec,omitempty"`
	Decider  string             `json:"decider"`
}

// ToolPolicyFact is one included harness's tool policy: the lane its calls take
// (gated, ungated, attended or unsupported), who enforces it (wardyn_gate,
// harness_settings, mcp_broker — the broker, for a harness whose MCP tools are
// its only rules — or none), its default, the policy's locked rules, the effects
// a person may choose for a tool the policy does not name, whether rules may
// name any tool or MCP tools only, and the MCP servers turned on for it.
type ToolPolicyFact struct {
	Harness     string           `json:"harness"`
	Lane        string           `json:"lane"`
	Enforcer    string           `json:"enforcer"`
	Default     ToolDefaultFact  `json:"default"`
	PolicyRules []LockedToolRule `json:"policy_rules"`
	RuleEffects []EffectOption   `json:"rule_effects"`
	RuleTools   string           `json:"rule_tools"`
	MCPServers  []string         `json:"mcp_servers"`
	// InnerWalls is the harness's own walls this run launches it with (#2822); nil for an exec run.
	InnerWalls *types.HarnessInnerWalls `json:"inner_walls,omitempty"`
}

// ToolDefaultFact is a harness's default for a tool no rule names.
type ToolDefaultFact struct {
	Effective types.ToolEffect `json:"effective"`
	Source    ProvenanceSource `json:"source"`
	Options   []EffectOption   `json:"options"`
}

// EffectOption is one effect a person may choose, or why they may not.
type EffectOption struct {
	Effect    types.ToolEffect `json:"effect"`
	Available bool             `json:"available"`
	Reason    string           `json:"reason,omitempty"`
}

// LockedToolRule is a rule the policy sets and the person cannot change.
type LockedToolRule struct {
	Tool   string           `json:"tool"`
	Effect types.ToolEffect `json:"effect"`
	Source ProvenanceSource `json:"source"`
}
