// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// stagedSet writes a set directory holding a spool and its .consumed sidecar and the manifest
// -recovery-manifest makes for the rig's target, then points the manifest's source at a database the
// target is not (the target stands for the loaded dump, the source for the deployment it was taken from).
func (rg *rehearseRig) stagedSet() string { return rg.stagedSetWith("pg", nil) }

// stagedSetWith is stagedSet under a recordings store, with fill adding the staged recordings and drive
// directories before the manifest digests them.
func (rg *rehearseRig) stagedSetWith(recordingStore string, fill func(dir string)) string {
	rg.t.Helper()
	dir := rg.t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "audit"), 0o700); err != nil {
		rg.t.Fatal(err)
	}
	if fill != nil {
		fill(dir)
	}
	writeFixtureFile(rg.t, filepath.Join(dir, "audit", "spool.jsonl"), "{\"id\":1}\n")
	writeFixtureFile(rg.t, filepath.Join(dir, "audit", "spool.jsonl.consumed"), "9\n")
	in := manifestSources{DSN: rg.targetDSN, Set: dir, SpoolBase: "spool.jsonl", AgeKey: rg.id.String(), KEKMode: "local", RecordingStore: recordingStore}
	if err := createRecoveryManifest(context.Background(), in); err != nil {
		rg.t.Fatalf("create the manifest: %v", err)
	}
	rg.editManifest(dir, func(m *recoveryManifest) {
		m.Source = recoverySource{Host: "source.invalid", Port: 5432, Database: "wardyn"}
	})
	return dir
}

func (rg *rehearseRig) editManifest(dir string, edit func(*recoveryManifest)) {
	rg.t.Helper()
	path := filepath.Join(dir, recoveryManifestFile)
	m, err := readRecoveryManifest(dir, "verify the set")
	if err != nil {
		rg.t.Fatal(err)
	}
	edit(&m)
	body, _ := json.Marshal(m)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		rg.t.Fatal(err)
	}
}

// runMode is `wardynd -recovery-rehearse` as main dispatches it.
func (rg *rehearseRig) runMode(ageKey, dir, rehearseDSN string, extra ...string) error {
	rg.t.Helper()
	rg.t.Setenv(rehearseDSNEnv, rehearseDSN)
	f := rg.flagsArgs(ageKey, append([]string{"-recovery-rehearse", "-recovery-set", dir}, extra...)...)
	ran, err := maintenanceMode(f)
	if !ran {
		rg.t.Fatal("-recovery-rehearse did not select a maintenance mode")
	}
	return err
}

func TestRecoveryRehearseMode_PassesAStagedSetAndWritesBothAuditRows(t *testing.T) {
	rg := newRehearseRig(t)
	dir := rg.stagedSet()
	if err := rg.runMode(rg.id.String(), dir, rg.targetDSN); err != nil {
		t.Fatalf("rehearse = %v", err)
	}
	if rows := rg.auditRows(); len(rows) != 2 || !strings.HasPrefix(rows[0], "recovery.rehearse.begin") || !strings.HasPrefix(rows[1], "recovery.rehearse success") {
		t.Errorf("audit rows = %v; want the begin row then a success verdict", rows)
	}
}

func TestRecoveryRehearseMode_ASidecarThatDiffersRefusesNamingItBeforeAnyAuditRow(t *testing.T) {
	rg := newRehearseRig(t)
	dir := rg.stagedSet()
	writeFixtureFile(t, filepath.Join(dir, "audit", "spool.jsonl.consumed"), "10\n")
	err := rg.runMode(rg.id.String(), dir, rg.targetDSN)
	requireExit(t, err, exitMigrateRefused, "spool.jsonl.consumed")
	if rows := rg.auditRows(); len(rows) != 0 {
		t.Errorf("a refused pairing wrote audit rows %v; nothing may be decrypted or recorded", rows)
	}
}

func TestRecoveryRehearseMode_ASidecarThatIsMissingRefuses(t *testing.T) {
	rg := newRehearseRig(t)
	dir := rg.stagedSet()
	if err := os.Remove(filepath.Join(dir, "audit", "spool.jsonl")); err != nil {
		t.Fatal(err)
	}
	requireExit(t, rg.runMode(rg.id.String(), dir, rg.targetDSN), exitMigrateRefused, "spool.jsonl")
}

func TestRecoveryRehearseMode_ADatabaseAtAnotherAuditHeadRefusesNamingIt(t *testing.T) {
	rg := newRehearseRig(t)
	dir := rg.stagedSet()
	rg.editManifest(dir, func(m *recoveryManifest) { m.Snapshot.AuditHeadSeq++ })
	requireExit(t, rg.runMode(rg.id.String(), dir, rg.targetDSN), exitMigrateRefused, "audit head")
}

func TestRecoveryRehearseMode_TheLiveDatabaseAsTargetRefuses(t *testing.T) {
	rg := newRehearseRig(t)
	dir := rg.stagedSet()
	requireExit(t, rg.runMode(rg.id.String(), dir, rg.liveDSN), exitMigrateRefused)
	if rows := rg.auditRows(); len(rows) != 0 {
		t.Errorf("a refused target wrote audit rows %v", rows)
	}
}

func TestRecoveryRehearseMode_AWrongKeyExitsFour(t *testing.T) {
	rg := newRehearseRig(t)
	dir := rg.stagedSet()
	requireExit(t, rg.runMode(mustAgeIdentity(t).String(), dir, rg.targetDSN), exitRehearseFail)
}

func TestRecoveryRehearseMode_AKEKModeOtherThanTheManifestsFailsBeforeAnyRow(t *testing.T) {
	rg := newRehearseRig(t)
	dir := rg.stagedSet()
	rg.editManifest(dir, func(m *recoveryManifest) { m.Keys.KEKMode = "transit" })
	err := rg.runMode(rg.id.String(), dir, rg.targetDSN)
	requireExit(t, err, exitRehearseFail)
}

func TestRecoveryRehearseMode_NeedsTheScratchDSNAndTheSet(t *testing.T) {
	rg := newRehearseRig(t)
	dir := rg.stagedSet()
	err := rg.runMode(rg.id.String(), dir, "")
	requireExit(t, err, 1, rehearseDSNEnv)

	rg.t.Setenv(rehearseDSNEnv, rg.targetDSN)
	f := rg.flagsArgs(rg.id.String(), "-recovery-rehearse")
	_, err = maintenanceMode(f)
	requireExit(t, err, 1, "-recovery-set")
}

func TestRecoveryRehearseMode_RunsAloneWithNoOtherVerb(t *testing.T) {
	rg := newRehearseRig(t)
	dir := rg.stagedSet()
	err := rg.runMode(rg.id.String(), dir, rg.targetDSN, "-recovery-verify")
	if err == nil || !strings.Contains(err.Error(), "runs alone") {
		t.Fatalf("err = %v; want the verbs refused together", err)
	}
	if rows := rg.auditRows(); len(rows) != 0 {
		t.Errorf("a refused combination wrote audit rows %v", rows)
	}
}

// The manifest's key section is built from configuration and the dump, and holds no key material.
func TestRecoveryManifest_NoSecretValueInManifestOrOutput(t *testing.T) {
	rg := newRehearseRig(t)
	dir := rg.stagedSet()
	body, err := os.ReadFile(filepath.Join(dir, recoveryManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	m, err := readRecoveryManifest(dir, "verify the set")
	if err != nil {
		t.Fatal(err)
	}
	if m.Keys.AgeRecipient != rg.id.Recipient().String() || m.Keys.KEKMode != "local" || len(m.Keys.Classes) == 0 {
		t.Errorf("keys = %+v; want the recipient, the mode and the class counts", m.Keys)
	}
	for _, c := range []string{rg.id.String(), canaryLivePW, canaryTargetPW, canaryValue, canaryLegacy} {
		if strings.Contains(string(body), c) {
			t.Errorf("manifest holds the canary %q", c)
		}
	}
}

// With WARDYN_KEK=transit the manifest is written from configuration and the database alone: the Vault
// the deployment names is never called.
func TestRecoveryManifest_NeverCallsTheKeyService(t *testing.T) {
	rg := newRehearseRig(t)
	var calls atomic.Int32
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected call", http.StatusTeapot)
	}))
	defer vault.Close()
	for k, v := range map[string]string{"WARDYN_KEK": "transit", "WARDYN_VAULT_ADDR": vault.URL, "WARDYN_VAULT_TRANSIT_KEY": "wardyn"} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "audit"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := rg.flagsArgs(rg.id.String(), "-recovery-manifest", "-recovery-set", dir)
	in := manifestSourcesOf(f, dir)
	in.DSN = rg.targetDSN
	if err := createRecoveryManifest(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	m, err := readRecoveryManifest(dir, "verify the set")
	if err != nil {
		t.Fatal(err)
	}
	if m.Keys.KEKMode != "transit" || calls.Load() != 0 {
		t.Errorf("kek mode %q, %d key-service call(s); want transit and none", m.Keys.KEKMode, calls.Load())
	}
}
