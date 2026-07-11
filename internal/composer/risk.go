// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RiskLevel is Wardyn's deterministic grade for a single config choice.
type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// rank orders levels for aggregation (higher = riskier).
func (l RiskLevel) rank() int {
	switch l {
	case RiskHigh:
		return 3
	case RiskMedium:
		return 2
	case RiskLow:
		return 1
	default:
		return 0
	}
}

// RiskItem is one graded config choice. Field/Value identify what was graded,
// Level is Wardyn's deterministic grade, Rationale explains it to the human, and
// InvariantRef (optional) cites the security invariant the choice bears on.
type RiskItem struct {
	Field        string    `json:"field"`
	Value        string    `json:"value"`
	Level        RiskLevel `json:"risk_level"`
	Rationale    string    `json:"rationale"`
	InvariantRef string    `json:"invariant_ref,omitempty"`
}

// safeBaselineDomains are egress hosts a coding agent commonly needs; allowlisting
// only these is not, by itself, escalated. Anything BEYOND this set is graded
// medium so the operator notices custom egress.
var safeBaselineDomains = map[string]bool{
	// LLM provider endpoints a coding agent commonly needs for its OWN model
	// access (the proposed run's provider) — not over-flagged as custom egress.
	"api.anthropic.com":                 true,
	"api.openai.com":                    true,
	"generativelanguage.googleapis.com": true,
	// VCS + package registries. This set is kept in sync with the egress hosts
	// the workspace scanner's filename-keyed marker table (internal/
	// workspacescan/markers.go) can attach — those are all standard registries
	// an onboarded workspace legitimately unions into a run, so a scan-derived
	// egress addition never reads as "custom" medium-risk egress.
	"github.com":                    true,
	"api.github.com":                true,
	"codeload.github.com":           true,
	"objects.githubusercontent.com": true,
	"registry.npmjs.org":            true,
	"registry.yarnpkg.com":          true,
	"pypi.org":                      true,
	"files.pythonhosted.org":        true,
	"proxy.golang.org":              true,
	"sum.golang.org":                true,
	// JVM (maven/gradle), rust, ruby, php, .net, elixir, dart — the rest of the
	// marker table's egress vars.
	"repo.maven.apache.org": true,
	"repo1.maven.org":       true,
	"plugins.gradle.org":    true,
	"services.gradle.org":   true,
	"crates.io":             true,
	"static.crates.io":      true,
	"index.crates.io":       true,
	"rubygems.org":          true,
	"repo.packagist.org":    true,
	"api.nuget.org":         true,
	"hex.pm":                true,
	"pub.dev":               true,
}

// Grade computes the deterministic risk assessment of a proposed run setup PURELY
// from its fields. It NEVER consults any LLM self-assessment — a prompt-injected
// attachment cannot lower the grade because the grade is a function of the spec,
// not of anything the model claims about it. It emits one item per notable choice
// (including LOW ones) so the human sees the full picture, sorted riskiest-first.
func Grade(run RunInput, spec types.RunPolicySpec) []RiskItem {
	var items []RiskItem
	add := func(field, value string, lvl RiskLevel, rationale, inv string) {
		items = append(items, RiskItem{Field: field, Value: value, Level: lvl, Rationale: rationale, InvariantRef: inv})
	}

	// ── Confinement class: CC1 weakest (runc) → CC2 (gVisor) → CC3 (Kata). ──
	switch strings.ToUpper(strings.TrimSpace(string(spec.MinConfinementClass))) {
	case string(types.CC1):
		add("min_confinement_class", "CC1", RiskHigh,
			"Fence (the weakest tier — a hardened shared-kernel container): a kernel-level escape is not contained by a second boundary.", "5")
	case string(types.CC3):
		add("min_confinement_class", "CC3", RiskLow,
			"Vault (the strongest tier — a hardware-isolated Kata VM).", "5")
	case string(types.CC2):
		add("min_confinement_class", "CC2", RiskMedium,
			"Wall (the default tier — a gVisor sandbox).", "5")
	default:
		add("min_confinement_class", string(spec.MinConfinementClass), RiskMedium,
			"Unrecognized confinement class; treated as medium pending validation.", "5")
	}

	// ── Egress posture. ──
	switch {
	case spec.AllowAllEgress:
		add("allow_all_egress", "true", RiskHigh,
			"Allow-all egress lets the agent reach ANY public host (deny-list only). Exfiltration surface is maximal; private/metadata IPs are still blocked structurally.", "3")
	default:
		extra := beyondBaseline(spec.AllowedDomains)
		if len(extra) > 0 {
			add("allowed_domains", strings.Join(extra, ","), RiskMedium,
				fmt.Sprintf("Custom egress to %d host(s) beyond the safe baseline.", len(extra)), "3")
		} else if len(spec.AllowedDomains) > 0 {
			add("allowed_domains", strings.Join(spec.AllowedDomains, ","), RiskLow,
				"Default-deny egress limited to baseline coding-agent hosts.", "3")
		}
	}
	// first_use_approval interacts with a non-trivial allowlist (inert under allow-all).
	if !spec.AllowAllEgress && len(spec.AllowedDomains) > 0 {
		switch spec.FirstUseApproval.Normalize() {
		case types.FirstUseWaitForReview:
			add("first_use_approval", "wait_for_review", RiskLow,
				"Unknown domains pause and wait for you to approve them live before the agent proceeds.", "3")
		case types.FirstUseDenyWithReview:
			add("first_use_approval", "deny_with_review", RiskLow,
				"Unknown domains escalate to a human instead of silently failing.", "3")
		default: // always_deny
			add("first_use_approval", "always_deny", RiskMedium,
				"Unknown domains are hard-denied (no human escalation); the agent may fail opaquely on a needed host.", "3")
		}
	}

	// ── Credential grants. ──
	for i, g := range spec.EligibleGrants {
		gradeGrant(add, i, g)
	}

	// ── Workspace mounts: read-write host bind = host writes persist. ──
	for i, m := range spec.WorkspaceMounts {
		ro := m.ReadOnlyOrDefault()
		field := fmt.Sprintf("workspace_mounts[%d]", i)
		val := fmt.Sprintf("%s→%s (%s)", m.Source, m.Target, roLabel(ro))
		if ro {
			add(field, val, RiskLow, "Read-only host mount; the agent cannot modify host files.", "1")
		} else {
			add(field, val, RiskHigh,
				"Read-WRITE host mount: the agent's writes persist to the host filesystem.", "1")
		}
	}

	// ── Idle reaping. ──
	if spec.AutoStopAfterSec < 0 {
		if run.Interactive {
			add("auto_stop_after_sec", fmt.Sprintf("%d", spec.AutoStopAfterSec), RiskLow,
				"Never-reap is expected for an interactive run (it comes up idle awaiting a human attach).", "")
		} else {
			add("auto_stop_after_sec", fmt.Sprintf("%d", spec.AutoStopAfterSec), RiskHigh,
				"Never-reap on a NON-interactive run: a forgotten run holds its minted credentials indefinitely.", "6")
		}
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].Level.rank() > items[j].Level.rank() })
	return items
}

// gradeGrant grades one credential grant by kind + parsed scope.
func gradeGrant(add func(field, value string, lvl RiskLevel, rationale, inv string), i int, g types.GrantSpec) {
	field := fmt.Sprintf("eligible_grants[%d]", i)
	switch g.Kind {
	case types.GrantGitHubToken:
		writers := githubWritePerms(g.Scope)
		if len(writers) > 0 {
			lvl := RiskHigh
			rat := fmt.Sprintf("GitHub token with WRITE permission(s) %s.", strings.Join(writers, ","))
			if !g.RequiresApproval {
				rat += " requires_approval=false: it would auto-mint a write-capable credential with no human in the loop."
			}
			add(field, "github_token (write)", lvl, rat, "2")
		} else {
			add(field, "github_token (read)", RiskMedium,
				"GitHub token with read-only permissions.", "2")
		}
	case types.GrantAPIKey:
		add(field, "api_key", RiskMedium,
			"Third-party API key injected proxy-side; scoped to the injection host.", "1")
	case types.GrantCloudSTS:
		add(field, "cloud_sts", RiskHigh,
			"Cloud STS grant confers broad cloud credentials (and requires a SPIRE identity provider; the embedded IdP refuses it).", "5")
	case types.GrantGitPAT:
		add(field, "git_pat", RiskHigh,
			"Stored PAT handed to git (brokered stdout-only, never resident) — but Wardyn cannot expire or down-scope it; blast radius is whatever the token grants. Prefer a GitHub App, or a fine-grained repo-scoped short-expiry token.", "2")
	case types.GrantSSHKey:
		add(field, "ssh_key", RiskHigh,
			"Resident, agent-readable SSH private key for the clone window (git's SSH transport has no broker seam) — Wardyn can neither scope nor expire it. Prefer a single-repo read-only deploy key over a personal identity.", "2")
	default:
		add(field, string(g.Kind), RiskMedium, "Unrecognized grant kind; treated as medium.", "")
	}
	// A write-capable grant that does not require approval is independently HIGH.
	if !g.RequiresApproval && grantIsWriteCapable(g) {
		add(field+".requires_approval", "false", RiskHigh,
			"A write-capable grant with requires_approval=false auto-mints with no human approval.", "2")
	} else if g.RequiresApproval {
		add(field+".requires_approval", "true", RiskLow,
			"Minting this grant requires explicit human approval.", "2")
	}
}

// githubWritePerms returns the permission names set to "write"/"admin" in a
// github grant scope ({"repos":[...],"permissions":{"contents":"write",...}}).
func githubWritePerms(scope json.RawMessage) []string {
	var s struct {
		Permissions map[string]string `json:"permissions"`
	}
	if len(scope) == 0 {
		return nil
	}
	if err := json.Unmarshal(scope, &s); err != nil {
		return nil
	}
	var w []string
	for perm, level := range s.Permissions {
		l := strings.ToLower(level)
		if l == "write" || l == "admin" {
			w = append(w, perm+":"+l)
		}
	}
	sort.Strings(w)
	return w
}

func grantIsWriteCapable(g types.GrantSpec) bool {
	switch g.Kind {
	case types.GrantGitHubToken:
		return len(githubWritePerms(g.Scope)) > 0
	case types.GrantCloudSTS:
		return true
	default:
		// ponytail: git_pat/ssh_key grade High in gradeGrant but are deliberately
		// NOT write-capable here — their scope carries no read/write flag, and an
		// unconditional CC3 floor (RequiredConfinementFloor) would block every SCM
		// clone on KVM-less hosts. Add a `readonly` bool to those scopes and floor
		// only declared-write grants if that ever changes.
		return false
	}
}

// RequiredConfinementFloor returns the DETERMINISTIC minimum confinement class a
// run's BLAST RADIUS requires — independent of what the model proposed or the
// operator picked. A run that holds POWERFUL credentials is itself a high-value
// compromise target: if a prompt-injected agent escapes the sandbox, it takes
// those credentials (and your host) with it. Such a run must therefore run in the
// STRONGEST sandbox (Vault / CC3) so an escape is contained. "Powerful" means the
// run can mutate external/production systems or authenticate to third-party
// services:
//   - a WRITE-CAPABLE grant (cloud STS, or a GitHub token with write/admin), or
//   - an api_key to a host OUTSIDE the safe coding-agent baseline — i.e. a
//     database, deploy API, or other third-party production credential (the
//     agent's own model/VCS api_keys are baseline and do NOT floor).
//
// Returns "" when no floor above the policy default applies. Enforced BOTH in the
// composer proposal and (defense-in-depth) at run.create, where a host that can't
// provide CC3 then fails closed rather than running the workload under-confined.
func RequiredConfinementFloor(spec types.RunPolicySpec) types.ConfinementClass {
	for _, g := range spec.EligibleGrants {
		if grantIsWriteCapable(g) || apiKeyToNonBaselineHost(g) {
			return types.CC3
		}
	}
	return ""
}

// apiKeyToNonBaselineHost reports whether an api_key grant targets a host OUTSIDE
// the safe coding-agent baseline (the agent's own model/VCS endpoints) — i.e. a
// credential to a third-party / production service (a database, deploy API, SaaS).
// An unparseable/empty host is treated as baseline (no floor) — the floor keys off
// a POSITIVE signal, never an absence.
func apiKeyToNonBaselineHost(g types.GrantSpec) bool {
	if g.Kind != types.GrantAPIKey || len(g.Scope) == 0 {
		return false
	}
	var s struct {
		Host string `json:"host"`
	}
	if err := json.Unmarshal(g.Scope, &s); err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(s.Host))
	return host != "" && !safeBaselineDomains[host]
}

// beyondBaseline returns allowlisted domains not in the safe baseline.
func beyondBaseline(domains []string) []string {
	var extra []string
	for _, d := range domains {
		if !safeBaselineDomains[strings.ToLower(strings.TrimSpace(d))] {
			extra = append(extra, d)
		}
	}
	sort.Strings(extra)
	return extra
}

func roLabel(ro bool) string {
	if ro {
		return "ro"
	}
	return "rw"
}

// OverallLevel returns the highest level among items (low if none).
func OverallLevel(items []RiskItem) RiskLevel {
	max := RiskLow
	for _, it := range items {
		if it.Level.rank() > max.rank() {
			max = it.Level
		}
	}
	return max
}
