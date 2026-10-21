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

// auditHostShapedFields are the data field names that carry host detail: a host path, a cluster storage
// class, a source path, a node or machine name, a substrate object name. A field of one of these names on any
// action's row is a decision someone must write down, in auditHostViews or auditHostReviewed.
var auditHostShapedFields = []string{"branding_logo_path", "host_path", "host_root", "locator", "machine",
	"mount_source", "node", "node_name", "object", "source_id", "storage_class", "workspace_mounts"}

// auditHostShapedText marks a row whose cell describes host detail in prose: a policy snapshot carries
// workspace_mounts[].source, and host:port names a database host.
var auditHostShapedText = []string{"policy snapshot", "host:port"}

// auditHostReviewed are the actions with a host-shaped field that are served as recorded, and why.
var auditHostReviewed = map[string]string{
	"run.node.record":             "source_id is the security tier's by design (the containment read); withheld below it by withoutNodeRecordSource",
	"run.drive.mount":             "object and target name a Wardyn volume or claim, or <drive>/<home> for a host_path drive, never the host root",
	"drive.reclaim":               "object is a Wardyn-named volume or claim; a host_path drive is refused before any object",
	"drive.member.delete":         "object is a Wardyn-named volume or claim",
	"cli.session.create":          "machine is the person's own machine label, not the deployment's host",
	"cli.session.revoke":          "machine is the person's own machine label",
	"cli.sign_in.request":         "machine is the person's own machine label",
	"cli.sign_in.approve":         "machine is the person's own machine label",
	"policy.inline.apply":         "workspace_mounts is a count of mounts, never a path",
	"cli.session.refresh_refused": "machine is the person's own machine label",
	"cli.session.reuse_detected":  "machine is the person's own machine label",
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

// TestAuditHostDetailCensus walks every documented audit action and fails on one whose data carries a
// host-shaped field that is neither projected (auditHostViews) nor reviewed (auditHostReviewed), on an
// allow-list that serves a host-shaped field, and on a projection or review naming no documented action.
func TestAuditHostDetailCensus(t *testing.T) {
	rows := auditDocFields(t, readAuditActionsDoc(t))
	for action, cell := range rows {
		var host []string
		for _, m := range auditDocBacktick.FindAllStringSubmatch(cell, -1) {
			if slices.Contains(auditHostShapedFields, m[1]) {
				host = append(host, m[1])
			}
		}
		for _, s := range auditHostShapedText {
			if strings.Contains(cell, s) {
				host = append(host, s)
			}
		}
		if len(host) == 0 {
			continue
		}
		view, projected := auditHostViews[action]
		if _, reviewed := auditHostReviewed[action]; !projected && !reviewed {
			t.Errorf("audit action %q records host-shaped data %v but is not classified: add it to auditHostViews (withheld below a super admin) or auditHostReviewed (served, with why)", action, host)
		}
		for _, k := range host {
			if slices.Contains(view.allow, k) {
				t.Errorf("audit action %q serves host-shaped field %q on its allow-list", action, k)
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
