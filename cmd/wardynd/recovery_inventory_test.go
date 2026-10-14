// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// recoveryFlagsFor is the bootFlags a `wardynd -recovery-inventory` run resolves for one deployment, with
// every knob the inventory reads set. Tests build it by hand: parseBootFlags reads the process flags.
func recoveryFlagsFor(runner, dsn, spool, recording string) *bootFlags {
	s := func(v string) *string { return &v }
	b := func(v bool) *bool { return &v }
	return &bootFlags{
		dsn:                 s(dsn),
		runnerSel:           s(runner),
		auditSpool:          s(spool),
		auditSinks:          s(""),
		recordingSel:        s(recording),
		recordingDir:        s("/data/recordings"),
		ageKey:              s(""),
		platformKeyFile:     s(""),
		vault:               vaultFlags{kek: s("local")},
		rotateAgeKey:        s(""),
		rewrap:              b(false),
		rewrapPrincipalKeys: b(false),
		migrateSecrets:      b(false),
		reconcile:           b(false),
		auditSplitLegacy:    b(false),
		migrateOnly:         b(false),
		recoveryInventory:   b(true),
		recoveryManifest:    b(false),
		recoveryVerify:      b(false),
		recoveryRehearse:    b(false),
		recoverySet:         s(""),
	}
}

// inventoryJSON runs the inventory for one deployment and returns its decoded report.
func inventoryJSON(t *testing.T, f *bootFlags, managedDir string) recoveryInventoryReport {
	t.Helper()
	var buf bytes.Buffer
	if err := writeRecoveryInventory(&buf, f, managedDir); err != nil {
		t.Fatalf("inventory: %v", err)
	}
	var rep recoveryInventoryReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("inventory is not JSON: %v\n%s", err, buf.String())
	}
	return rep
}

func TestRecoveryInventory_ListsEveryItemWithALocation(t *testing.T) {
	const dsn = "postgres://wardyn@db.internal:5432/wardyn?sslmode=disable"
	cases := []struct {
		name, runner, managedDir, deployment string
	}{
		{"compose", "docker", "", recoveryDeployCompose},
		{"desktop", "docker", "/etc/wardyn", recoveryDeployDesktop},
		{"helm", "k8s", "", recoveryDeployHelm},
	}
	wantItems := []string{"database", "audit_spool", "recordings", "drives", "keys", "manifest"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := inventoryJSON(t, recoveryFlagsFor(tc.runner, dsn, "/data/audit/audit-spool.jsonl", "fs"), tc.managedDir)
			if rep.Deployment != tc.deployment {
				t.Fatalf("deployment = %q, want %q", rep.Deployment, tc.deployment)
			}
			if len(rep.Items) != len(wantItems) {
				t.Fatalf("%d items, want %d: %+v", len(rep.Items), len(wantItems), rep.Items)
			}
			for i, it := range rep.Items {
				if it.Item != wantItems[i] {
					t.Errorf("item %d = %q, want %q", i, it.Item, wantItems[i])
				}
				if strings.TrimSpace(it.Location) == "" {
					t.Errorf("item %q has no location", it.Item)
				}
			}
		})
	}
}

func TestRecoveryInventory_SpoolAndRecordingsFollowTheResolvedFlags(t *testing.T) {
	f := recoveryFlagsFor("docker", "postgres://wardyn@db.internal/wardyn", "", "off")
	rep := inventoryJSON(t, f, "")
	if got := rep.Items[1]; got.Item != "audit_spool" || !strings.HasPrefix(got.Location, "none") {
		t.Errorf("empty WARDYN_AUDIT_SPOOL: audit_spool = %+v, want none", got)
	}
	if got := rep.Items[2]; got.Item != "recordings" || !strings.HasPrefix(got.Location, "none") {
		t.Errorf("recording store off: recordings = %+v, want none", got)
	}
	f = recoveryFlagsFor("docker", "postgres://wardyn@db.internal/wardyn", "/data/audit/audit-spool.jsonl", "fs")
	rep = inventoryJSON(t, f, "")
	if got := rep.Items[1].SetPath; got != "audit/audit-spool.jsonl" {
		t.Errorf("audit_spool set path = %q, want audit/audit-spool.jsonl", got)
	}
	if got := rep.Items[2].SetPath; got != "recordings/" {
		t.Errorf("fs recordings set path = %q, want recordings/", got)
	}
}

// TestRecoveryInventory_NeverPrintsASecretValue puts canaries where a secret would be: the age key, the
// DSN password and a sink token. None may reach the report. Mutation: make dsnDisplay return the DSN as
// given, and the DSN canary appears in the output.
func TestRecoveryInventory_NeverPrintsASecretValue(t *testing.T) {
	const (
		agePayload = "AGE-SECRET-KEY-1CANARYAGEKEYVALUEDONOTPRINT"
		dsnPass    = "dsnpwCANARYdsnpw"
		sinkToken  = "sinktokenCANARY"
	)
	dsn := "postgres://wardyn:" + dsnPass + "@db.internal:5432/wardyn?sslmode=disable&application_name=" + sinkToken
	f := recoveryFlagsFor("k8s", dsn, "/data/audit/audit-spool.jsonl", "pg")
	*f.ageKey = agePayload
	*f.auditSinks = "https://sink.example/ingest?token=" + sinkToken
	var buf bytes.Buffer
	if err := writeRecoveryInventory(&buf, f, ""); err != nil {
		t.Fatalf("inventory: %v", err)
	}
	for _, canary := range []string{agePayload, dsnPass, sinkToken} {
		if strings.Contains(buf.String(), canary) {
			t.Fatalf("inventory printed a secret value (%q):\n%s", canary, buf.String())
		}
	}
	if !strings.Contains(buf.String(), "db.internal:5432/wardyn") {
		t.Errorf("inventory did not name the database host, port and name:\n%s", buf.String())
	}
}

// TestRecoveryInventory_OpensNoConnection points the database at a port nothing listens on: an inventory
// that dialled it would fail, so exit 0 proves it does not.
func TestRecoveryInventory_OpensNoConnection(t *testing.T) {
	f := recoveryFlagsFor("docker", "postgres://wardyn:x@127.0.0.1:1/wardyn?sslmode=disable", "/data/audit/audit-spool.jsonl", "fs")
	ran, err := recoveryMode(f)
	if !ran || err != nil {
		t.Fatalf("recoveryMode(-recovery-inventory) = ran %v, err %v; want ran and no error", ran, err)
	}
}

func TestRecoveryMode_RefusesACombinedMaintenanceMode(t *testing.T) {
	f := recoveryFlagsFor("docker", "postgres://wardyn@127.0.0.1:1/wardyn", "", "pg")
	*f.reconcile = true
	ran, err := recoveryMode(f)
	if !ran || err == nil {
		t.Fatalf("recovery verb with -reconcile: ran %v, err %v; want a refusal", ran, err)
	}
}
