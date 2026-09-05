// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// TestAuditSpoolEphemeralWarnsAtBoot is F199.
//
// auditspool.go and docs/OPERATIONS.md both promised, without qualification,
// that the quarantine alert survives a deploy / crash loop / pod reschedule. On
// the Helm chart's DEFAULT (persistence.enabled=false) WARDYN_AUDIT_SPOOL is
// /tmp/audit-spool.jsonl and /tmp is an emptyDir, so a rolling deploy discards
// the sidecar with the pod: the counter reads 0 again and the permanently
// refused events it accounts for are gone with it. Nothing said so anywhere an
// operator would look, and no scrape surface could tell them afterwards — a
// cleared counter and a genuinely clean deployment are byte-identical.
//
// The assertion is the BOOT LINE, because boot is the only moment the daemon
// can tell an operator which durability they actually bought.
func TestAuditSpoolEphemeralWarnsAtBoot(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// t.TempDir() is under the OS temp dir — the same shape the chart's default
	// install produces.
	if _, err := NewAuditSpool(filepath.Join(t.TempDir(), "audit-spool.jsonl")); err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "ephemeral storage") {
		t.Errorf("no ephemeral-storage warning for a spool under the temp dir: an operator on the chart's default "+
			"install is told nothing, and after a redeploy a cleared quarantine counter is indistinguishable from a "+
			"deployment that never quarantined anything.\nlog=%q", got)
	}
	if !strings.Contains(got, "persistence.enabled=true") {
		t.Errorf("the warning names no remedy, so it is an alarm an operator cannot act on; log=%q", got)
	}
}

// TestEphemeralSpoolDirUnderReports pins the heuristic's DIRECTION.
//
// An emptyDir is not a distinguishable filesystem — it is the node's disk, so
// statfs reports ext4/overlayfs and nothing unusual; there is no syscall that
// answers "will this survive a pod reschedule". The test is therefore a path
// convention, and the only safe way to be wrong is to stay quiet: a false alarm
// on a durable path teaches operators to ignore the line that matters.
func TestEphemeralSpoolDirUnderReports(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/tmp/audit-spool.jsonl", true},             // the chart's default
		{"/tmp/wardyn/audit-spool.jsonl", true},      // and any subdirectory of it
		{"/var/lib/wardyn/audit-spool.jsonl", false}, // persistence.enabled=true
		{"/data/audit-spool.jsonl", false},           // compose's named volume
		{"./data/audit-spool.jsonl", false},          // wardynd's own default
		{"/var/tmpfiles/audit-spool.jsonl", false},   // NOT /tmp: no false alarm on a prefix collision
	} {
		if _, got := ephemeralSpoolDir(tc.path); got != tc.want {
			t.Errorf("ephemeralSpoolDir(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
