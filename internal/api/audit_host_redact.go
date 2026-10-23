// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// auditRedactedFieldsKey names, inside a served row's data, the fields a reader below the super-admin tier was
// not given. Its presence is how a reader tells a projected row from a stored one: the row keeps its stored
// row_hash and prev_hash, so the chain still links across it, but its content cannot be re-hashed from what
// was served.
const auditRedactedFieldsKey = "redacted_fields"

// auditHostView is how one action's data is served below the super-admin tier: the host detail the
// super-only reads hold back (routes.go: GET /drives, /sources, /site-config) is withheld and everything
// else passes.
type auditHostView struct {
	// allow, when set, is every data key served; any other key, one added later included, is withheld.
	allow []string
	// withhold, when allow is nil, names the keys withheld from a document too wide to list (site config).
	withhold []string
	// mounts are the paths, from the top of data, to each policy spec whose workspace_mounts[].source is
	// blanked as redactSpecForUser does; an empty path is data itself.
	mounts [][]string
}

// auditHostViews is every action whose data carries host detail, keyed by canonical name. An action not
// here is served as recorded. TestAuditHostDetailCensus fails on a documented action with a host-shaped
// field that is neither withheld here nor reviewed.
var auditHostViews = map[string]auditHostView{
	// host_root and storage_class: what GET /drives keeps super-only.
	"drive.write": {allow: []string{"backend", "home_template", "name", "object_scheme", "reclaim", "rehomed",
		"rehomed_fields", "rehomed_subjects", "size_mib", "team", "writable"}},
	"drive.member.create": {allow: []string{"backend", "count", "drive", "drive_id", "limit", "size_mib"}},
	// locator: a local_dir source's path, the reason GET /sources is super-only.
	"source.write": {allow: []string{"kind", "ref", "writable"}},
	// target: the scratch database's host:port/database.
	"recovery.rehearse.begin": {allow: []string{"manifest_head_seq", "set_id"}},
	"recovery.rehearse":       {allow: []string{"checks", "manifest_head_seq", "set_id", "verdict"}},
	// What GET /site-config keeps super-only: the logo's host file, the internal host suffixes, the upstream
	// proxy and the egress redirects with their targets (go_checksum_db_off is the go redirects' targets).
	"site_config.write": {withhold: []string{"branding_logo_path", "egress_redirects", "go_checksum_db_off",
		"internal_hosts", "upstream_proxy_url"}},
	// The redirect probe's target, an egress redirect's to half.
	"site_config.redirect.test": {allow: []string{"elapsed_ms", "from_host", "state"}},
	// The host path behind each workspace mount of a recorded policy spec.
	"run.policy.resolve": {mounts: [][]string{{}}},
	"policy.create":      {mounts: [][]string{{"after"}}},
	"policy.update":      {mounts: [][]string{{"before", "spec"}, {"after", "spec"}}},
	"policy.delete":      {mounts: [][]string{{"before"}}},
	"preset.create":      {mounts: [][]string{{"request", "inline_policy"}, {"request", "policy_overlay"}}},
	"preset.update":      {mounts: [][]string{{"request", "inline_policy"}, {"request", "policy_overlay"}}},
}

// withoutHostDetail is evs as a reader below the super-admin tier is served them (auditHostViews). Only the
// data changes; the stored row, the sinks and a super admin's read keep it whole. It edits a copy, so a
// slice the caller still holds is never projected in place.
func withoutHostDetail(evs []types.AuditEvent) []types.AuditEvent {
	out := slices.Clone(evs)
	for i, ev := range out {
		if data, ok := hostViewOf(ev.Action, ev.Data); ok {
			out[i].Data = data
		}
	}
	return out
}

// hostViewOf is data as auditHostViews serves action's row, and whether anything was withheld. A row whose
// data does not decode is served with all of it withheld rather than unread.
func hostViewOf(action string, data []byte) (json.RawMessage, bool) {
	view, ok := auditHostViews[canonicalAction(action)]
	if !ok || len(data) == 0 || string(data) == "null" {
		return nil, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return mustJSON(map[string]any{auditRedactedFieldsKey: []string{"data"}}), true
	}
	var redacted []string
	for k := range m {
		if (view.allow != nil && !slices.Contains(view.allow, k)) || slices.Contains(view.withhold, k) {
			delete(m, k)
			redacted = append(redacted, k)
		}
	}
	for _, path := range view.mounts {
		if blankMountsAt(m, path) {
			redacted = append(redacted, strings.Join(append(slices.Clone(path), "workspace_mounts", "source"), "."))
		}
	}
	if len(redacted) == 0 {
		return nil, false
	}
	slices.Sort(redacted)
	m[auditRedactedFieldsKey] = mustJSON(redacted)
	return mustJSON(m), true
}

// blankMountsAt runs blankMountSources on the object path names inside m, re-encoding each level it
// changed; a level that is absent or not an object changes nothing.
func blankMountsAt(m map[string]json.RawMessage, path []string) bool {
	if len(path) == 0 {
		return blankMountSources(m)
	}
	var inner map[string]json.RawMessage
	if json.Unmarshal(m[path[0]], &inner) != nil || inner == nil || !blankMountsAt(inner, path[1:]) {
		return false
	}
	m[path[0]] = mustJSON(inner)
	return true
}

// blankMountSources sets each workspace_mounts[].source in m to "<redacted>", reporting whether one was set.
func blankMountSources(m map[string]json.RawMessage) bool {
	var mounts []map[string]json.RawMessage
	if json.Unmarshal(m["workspace_mounts"], &mounts) != nil {
		return false
	}
	changed := false
	for _, mt := range mounts {
		if _, has := mt["source"]; has {
			mt["source"], changed = mustJSON("<redacted>"), true
		}
	}
	if changed {
		m["workspace_mounts"] = mustJSON(mounts)
	}
	return changed
}
