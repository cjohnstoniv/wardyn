// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// auditHostShapedFields are the data field names that carry host detail by name: a host path, a cluster
// storage class, a source path, a node or machine name, a substrate object name.
var auditHostShapedFields = []string{"branding_logo_path", "host_path", "host_root", "locator", "machine",
	"mount_source", "node", "node_name", "object", "source_id", "storage_class", "workspace_mounts"}

// auditHostShapedParts are the name parts that mark a field as host-shaped wherever they appear in it.
var auditHostShapedParts = []string{"_path", "_root", "_dir", "host", "_url", "locator"}

// auditHostImageFields are the fields that carry a whole spec or a before/after image, which can hold a
// policy's workspace_mounts[].source.
var auditHostImageFields = []string{"after", "before", "spec", "request", "inline_policy", "policy_overlay"}

// auditHostShapedText marks a row whose cell describes host detail in prose, mapped to the field that
// carries it: a policy snapshot is data itself (workspace_mounts), and host:port is recovery's target.
var auditHostShapedText = map[string]string{"policy snapshot": "workspace_mounts", "host:port": "target"}

func auditHostShaped(k string) bool {
	if slices.Contains(auditHostShapedFields, k) || slices.Contains(auditHostImageFields, k) {
		return true
	}
	for _, p := range auditHostShapedParts {
		if strings.Contains(k, p) {
			return true
		}
	}
	return false
}

// auditHostKeep is a host-shaped field an action serves as recorded, and why.
type auditHostKeep struct {
	keys []string
	why  string
}

const (
	keepEgress  = "an egress destination: the security tier decides egress and holds the org allow/denylist"
	keepMachine = "the person's own machine label, not the deployment's host"
	keepObject  = "a Wardyn-named volume or claim; a host_path drive names <drive>/<home> or is refused first"
	keepSandbox = "a directory inside the sandbox, not on the host"
	keepValues  = "posture values (modes, ids, counts, booleans), never a path or a spec"
	keepRule    = "an approval rule as its security-tier author wrote it: kinds, repo and preset predicates, and the egress hosts and tools it approves; it holds no policy's workspace_mounts"
	keepSigning = keepEgress + "; no_aws_host and unsigned_host are aws_signing_reason values about such a host"
)

// auditHostReviewed are the host-shaped fields served as recorded, per action. A host-shaped field on a
// documented row must be withheld by auditHostViews or named here.
var auditHostReviewed = map[string]auditHostKeep{
	"approval.decide":                    {[]string{"host"}, keepEgress},
	"authz.denied":                       {[]string{"host"}, keepEgress},
	"branding.write":                     {[]string{"support_url"}, "the support link every console reader is shown"},
	"cli.session.create":                 {[]string{"machine"}, keepMachine},
	"cli.session.refresh_refused":        {[]string{"machine"}, keepMachine},
	"cli.session.reuse_detected":         {[]string{"machine"}, keepMachine},
	"cli.session.revoke":                 {[]string{"machine"}, keepMachine},
	"cli.sign_in.approve":                {[]string{"machine"}, keepMachine},
	"cli.sign_in.request":                {[]string{"machine"}, keepMachine},
	"component.delete":                   {[]string{"hosts"}, keepEgress},
	"component.write":                    {[]string{"hosts"}, keepEgress},
	"credential.delivery.write":          {[]string{"after", "before"}, keepValues},
	"credential.mint":                    {[]string{"host"}, keepEgress},
	"drive.member.delete":                {[]string{"object"}, keepObject},
	"drive.reclaim":                      {[]string{"object"}, keepObject},
	"egress.*":                           {[]string{"host"}, keepEgress},
	"github_pat.own.resolve":             {[]string{"host"}, keepEgress},
	"governance.approval_rule.delete":    {[]string{"spec"}, keepRule},
	"governance.approval_rule.write":     {[]string{"spec"}, keepRule},
	"governance.egress_baseline.write":   {[]string{"after", "before"}, "the baseline egress host sets, which the security tier writes"},
	"governance.unit.write":              {[]string{"contact_request_url"}, "the contact link a profile shows its members"},
	"harness.login.start":                {[]string{"sso_start_url"}, "the identity provider's sign-in start URL, an egress destination"},
	"llm.scan.*":                         {[]string{"host"}, keepEgress},
	"model_provider.write":               {[]string{"base_url_changed"}, "a boolean"},
	"policy.inline.apply":                {[]string{"workspace_mounts"}, "a count of mounts, never a path"},
	"remote_runner.people_policy.delete": {[]string{"after", "before"}, keepValues},
	"remote_runner.people_policy.set":    {[]string{"after", "before"}, keepValues},
	"run.artifact.redirect":              {[]string{"host"}, keepEgress},
	"run.aws_cli.configure":              {[]string{"aws_signing_host"}, keepEgress},
	"run.bedrock.configure":              {[]string{"hosts", "aws_signing_host", "no_aws_host", "unsigned_host"}, keepSigning},
	"run.ceiling.reassert":               {[]string{"dropped_injection_hosts"}, keepEgress},
	"run.component.attach":               {[]string{"hosts"}, keepEgress},
	"run.create": {[]string{"inline_policy", "policy_overlay", "spec"},
		"a boolean, the overlay's key names, and the starting spec recorded with each mount source already <redacted>"},
	"run.drive.mount":                {[]string{"object"}, keepObject},
	"run.egress.confine":             {[]string{"hosts"}, keepEgress},
	"run.folder.transfer":            {[]string{"remote_dir"}, keepSandbox},
	"run.git_pat.drop":               {[]string{"dropped_hosts"}, keepEgress},
	"run.injection.drop":             {[]string{"host"}, keepEgress},
	"run.node.record":                {[]string{"source_id"}, "the security tier's by design (the containment read); withheld below it by withoutNodeRecordSource"},
	"run.policy.resolve":             {[]string{"request"}, "a disk_mib_basis value"},
	"run.provider.admit":             {[]string{"host"}, keepEgress},
	"run.requirement.inject":         {[]string{"injected_hosts"}, keepEgress},
	"run.requirement.skip":           {[]string{"host", "hosts"}, keepEgress},
	"run.revive":                     {[]string{"dropped_injection_hosts"}, keepEgress},
	"run.secret_placeholder.resolve": {[]string{"host"}, keepEgress},
	"run.seed":                       {[]string{"remote_dir"}, keepSandbox},
	"run.seed.hold":                  {[]string{"remote_dir"}, keepSandbox},
	"run.ssh.drop":                   {[]string{"dropped_hosts"}, keepEgress},
	"run.subscription.inject":        {[]string{"host"}, keepEgress},
	"runner.trust.write":             {[]string{"after", "before"}, keepValues},
	"runner.update":                  {[]string{"after", "before"}, "the runner's resource limits, numbers"},
	"runner_pool.default.set":        {[]string{"after", "before"}, keepValues},
	"runner_pool.runner.attach":      {[]string{"remote_hosted", "self_hosted"}, "runner_kind values"},
	"runner_pool.use_policy.delete":  {[]string{"after", "before"}, keepValues},
	"runner_pool.use_policy.set":     {[]string{"after", "before"}, keepValues},
	"runners.enabled.set":            {[]string{"after", "before"}, "booleans"},
	"secret.read":                    {[]string{"host"}, keepEgress},
	"site_config.proxy.test":         {[]string{"target_host"}, "the host the proxy probe dialled, an egress destination; the proxy itself is not on the row"},
	"site_config.redirect.test":      {[]string{"from_host"}, "the public host a redirect replaces, an egress destination; to_host is withheld"},
	"site_config.write": {[]string{"internal_hosts_count", "policy_help_request_url", "request_url", "scm_hosts_count", "sign_in_help_url"},
		"two counts, and links every console or signed-out reader is shown"},
	"ui.authorize":             {[]string{"host"}, "the sandbox app's own host name the relay authorized"},
	"workspace.provider.admit": {[]string{"host"}, keepEgress},
	"workspace_provider.write": {[]string{"base_urls"}, "forge base URLs, the egress axis the security tier decides on"},
}

// withholds reports whether v withholds field k from a reader below the super-admin tier.
func (v auditHostView) withholds(k string) bool {
	if (v.allow != nil && !slices.Contains(v.allow, k)) || slices.Contains(v.withhold, k) {
		return true
	}
	for _, path := range v.mounts {
		if (len(path) == 0 && k == "workspace_mounts") || (len(path) > 0 && path[0] == k) {
			return true
		}
	}
	return false
}

var auditDocActionRow = regexp.MustCompile("^\\|\\s*`([a-z0-9_.*:-]+)`")
var auditDocBacktick = regexp.MustCompile("`([^`]+)`")

// auditDocFields reads docs/AUDIT-ACTIONS.md's registry (every action the code emits has a row there:
// cmd/wardynd's TestAuditActionsDoc_EveryEmitHasRow) into each action's Data-fields cell.
func auditDocFields(t *testing.T, doc string) map[string]string {
	t.Helper()
	body, _, ok := strings.Cut(doc, "## Renamed in 0.8")
	if !ok {
		t.Fatal(`docs/AUDIT-ACTIONS.md has no "## Renamed in 0.8" heading; re-anchor this census`)
	}
	rows := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		m := auditDocActionRow.FindStringSubmatch(line)
		if cells := strings.Split(line, "|"); m != nil && len(cells) > 3 {
			rows[m[1]] += cells[3]
		}
	}
	if len(rows) < 100 {
		t.Fatalf("parsed %d action rows out of docs/AUDIT-ACTIONS.md; the table shape changed", len(rows))
	}
	return rows
}

func readAuditActionsDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/AUDIT-ACTIONS.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAuditHostDetailCensus walks every documented audit action and fails on a host-shaped field (by name,
// by name part, a spec or before/after image, or prose) that auditHostViews does not withhold and
// auditHostReviewed does not keep, and on a projection or review naming no documented action.
func TestAuditHostDetailCensus(t *testing.T) {
	rows := auditDocFields(t, readAuditActionsDoc(t))
	for action, cell := range rows {
		var host []string
		for _, m := range auditDocBacktick.FindAllStringSubmatch(cell, -1) {
			if auditHostFieldName.MatchString(m[1]) && auditHostShaped(m[1]) && !slices.Contains(host, m[1]) {
				host = append(host, m[1])
			}
		}
		for text, k := range auditHostShapedText {
			if strings.Contains(cell, text) && !slices.Contains(host, k) {
				host = append(host, k)
			}
		}
		view, projected := auditHostViews[action]
		keep := auditHostReviewed[action]
		for _, k := range host {
			if !(projected && view.withholds(k)) && !slices.Contains(keep.keys, k) {
				t.Errorf("audit action %q records host-shaped field %q that is neither withheld (auditHostViews) nor reviewed (auditHostReviewed, with why)", action, k)
			}
		}
	}
	for action := range auditHostViews {
		if _, ok := rows[action]; !ok {
			t.Errorf("auditHostViews names %q, which docs/AUDIT-ACTIONS.md has no row for", action)
		}
	}
	for action := range auditHostReviewed {
		if _, ok := rows[action]; !ok {
			t.Errorf("auditHostReviewed names %q, which docs/AUDIT-ACTIONS.md has no row for", action)
		}
	}
}

var auditHostFieldName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
