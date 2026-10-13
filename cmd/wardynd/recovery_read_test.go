// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, recoveryManifestFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A refusal names the verb that refused: rehearse is not "verify the set".
func TestReadRecoveryManifest_RefusalNamesTheCallersVerb(t *testing.T) {
	dir := writeManifest(t, `{"manifest": 99}`)
	for _, verb := range []string{"rehearse", "verify the set"} {
		_, err := readRecoveryManifest(dir, verb)
		if err == nil || !strings.Contains(err.Error(), "refusing to "+verb+":") {
			t.Errorf("verb %q: err = %v, want it to name the verb", verb, err)
		}
	}
}

// A FIFO named manifest.json is refused, not waited on for a writer.
func TestReadRecoveryManifest_RefusesAFIFOWithoutHanging(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, recoveryManifestFile), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readRecoveryManifest(dir, "rehearse")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want a not-a-regular-file refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readRecoveryManifest blocked on a FIFO")
	}
}

// The manifest records a lower-case host, so the T1 comparison is the design's case-insensitive one.
func TestRecoverySourceOf_LowercasesTheHost(t *testing.T) {
	src, err := recoverySourceOf("host=DB.Example.ORG port=5432 dbname=wardyn user=u password=p sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if src.Host != "db.example.org" {
		t.Errorf("Host = %q, want db.example.org", src.Host)
	}
}

// A source port past 65535 is refused, not wrapped into uint16 (70000 would become 4464).
func TestRecoveryRehearseMode_RefusesAPortOutOfRange(t *testing.T) {
	dir := writeManifest(t, `{"manifest": 1, "source": {"host": "db", "port": 70000, "database": "wardyn"}}`)
	t.Setenv(rehearseDSNEnv, "postgres://u:p@127.0.0.1:1/scratch")
	f := recoveryFlagsFor("docker", "", "", "fs")
	*f.recoveryInventory, *f.recoveryRehearse, *f.recoverySet = false, true, dir
	err := recoveryRehearseMode(f)
	if err == nil || !strings.Contains(err.Error(), "refusing to rehearse") || !strings.Contains(err.Error(), "70000") {
		t.Fatalf("err = %v, want a refusal naming port 70000", err)
	}
}
