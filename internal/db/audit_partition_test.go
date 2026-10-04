// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"fmt"
	"strings"
	"testing"
)

const (
	auditConversionFile = "0111_audit_partitioned.sql"
	auditReplayableFile = "0112_audit_chain_partitioned.sql"
	auditRetentionFile  = "0123_audit_retention.sql"
	auditChainHeadFile  = "0130_audit_chain_head_from_meta.sql"
)

// TestChainTriggerReplaySetExcludesTheConversion pins the property the whole conversion rests on.
// db.replayTriggerMigrations re-executes every migration whose text contains "TRIGGER
// audit_events_chain", to restore a trigger an owner dropped. 0111 converts the table; re-running
// it would refuse every boot after the first. So it must never spell the trigger as DDL, and the
// replayable definition (0112) must be in the set so a restore puts the partition-aware body back
// AFTER 0047/0056/0057/0058 have replaced it with theirs.
func TestChainTriggerReplaySetExcludesTheConversion(t *testing.T) {
	names, err := triggerMigrationFiles(auditChainTrigger)
	if err != nil {
		t.Fatalf("triggerMigrationFiles: %v", err)
	}
	var hasConversion, hasReplayable bool
	for _, n := range names {
		hasConversion = hasConversion || n == auditConversionFile
		hasReplayable = hasReplayable || n == auditReplayableFile
	}
	if hasConversion {
		t.Errorf("the replay set %v contains %s: a boot-time trigger restore would re-run the conversion", names, auditConversionFile)
	}
	if !hasReplayable {
		t.Errorf("the replay set %v lacks %s: a restored trigger would be bound to a pre-partition function body", names, auditReplayableFile)
	}
	if names[len(names)-1] != auditChainHeadFile {
		t.Errorf("the replay set ends with %s, want %s last: whatever is last is the body the database is left with",
			names[len(names)-1], auditChainHeadFile)
	}
	if strings.Contains(readMigration(t, auditConversionFile), "TRIGGER "+auditChainTrigger) {
		t.Errorf("%s spells TRIGGER %s", auditConversionFile, auditChainTrigger)
	}
}

// chainFunctionBody returns the text of audit_events_chain's CREATE OR REPLACE FUNCTION statement
// as a migration file carries it.
func chainFunctionBody(t *testing.T, file string) string {
	t.Helper()
	body := readMigration(t, file)
	const start = "CREATE OR REPLACE FUNCTION @ns@.audit_events_chain()"
	i := strings.Index(body, start)
	if i < 0 {
		t.Fatalf("%s does not define audit_events_chain()", file)
	}
	j := strings.Index(body[i:], "$fn$, '@ns@'")
	if j < 0 {
		t.Fatalf("%s: the audit_events_chain() definition is not terminated as expected", file)
	}
	return body[i : i+j]
}

// TestChainFunctionBodyIsTheSameInBothFiles: 0111 defines the chain function so the guards work at
// commit, and 0112 defines it again as the replayable text. Two copies that disagree would mean a
// fresh install and a restored trigger run different code.
func TestChainFunctionBodyIsTheSameInBothFiles(t *testing.T) {
	a, b := chainFunctionBody(t, auditConversionFile), chainFunctionBody(t, auditReplayableFile)
	if a != b {
		t.Errorf("audit_events_chain() differs between %s and %s", auditConversionFile, auditReplayableFile)
	}
}

// TestAuditLockKeyLiteralsMatchTheGoConstants: a migration cannot import Go, so the lock keys the
// functions take are literals. A literal that drifts from its constant would make the database and the
// daemon serialize on different locks.
func TestAuditLockKeyLiteralsMatchTheGoConstants(t *testing.T) {
	for file, keys := range map[string][]int64{
		auditConversionFile: {AuditChainLockKey, AuditPartitionLockKey},
		auditReplayableFile: {AuditChainLockKey},
		auditRetentionFile:  {AuditChainLockKey, AuditPartitionLockKey},
	} {
		body := readMigration(t, file)
		for _, key := range keys {
			if want := fmt.Sprintf("pg_advisory_xact_lock(%d)", key); !strings.Contains(body, want) {
				t.Errorf("%s does not contain %q", file, want)
			}
		}
	}
}
