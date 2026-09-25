// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func keyFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "platform.key")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReadPlatformKey_RefusesAKeyThatSeparatesNothing: WARDYN_PLATFORM_KEY_FILE
// is only accepted as a durable second key of its own.
func TestReadPlatformKey_RefusesAKeyThatSeparatesNothing(t *testing.T) {
	ageID, _ := age.GenerateX25519Identity()
	platform, _ := age.GenerateX25519Identity()
	for label, c := range map[string]struct{ path, ageKey, want string }{
		"no age key":        {keyFile(t, platform.String()), "", "WARDYN_AGE_KEY is not"},
		"a missing file":    {filepath.Join(t.TempDir(), "absent"), ageID.String(), "does not exist"},
		"not an age key":    {keyFile(t, "WARDYN_AGE_KEY=x\n"), ageID.String(), "not an age key file"},
		"the age key again": {keyFile(t, "# created\n"+ageID.String()+"\n"), ageID.String(), "same key as WARDYN_AGE_KEY"},
		"a published key":   {keyFile(t, knownPublicAgeKeys[0]), ageID.String(), "publicly-known"},
	} {
		if _, err := readPlatformKey(c.path, c.ageKey); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: readPlatformKey = %v, want a refusal saying %q", label, err, c.want)
		}
	}
	if id, err := readPlatformKey("", ageID.String()); id != nil || err != nil {
		t.Errorf("unset = (%v, %v), want (nil, nil)", id, err)
	}
	id, err := readPlatformKey(keyFile(t, "# public key: x\n"+platform.String()+"\n"), ageID.String())
	if err != nil || id.String() != platform.String() {
		t.Fatalf("a second key = (%v, %v)", id, err)
	}
}

func mustAgeIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestRewrapMode_RefusesBeforeTouchingTheDatabase: -rewrap checks its inputs
// before db.Connect, against an unreachable DSN so a missed refusal fails on
// the connect instead of passing.
func TestRewrapMode_RefusesBeforeTouchingTheDatabase(t *testing.T) {
	const unreachableDSN = "postgres://nobody@127.0.0.1:1/nope?connect_timeout=1"
	good := mustAgeIdentity(t).String()
	for label, c := range map[string]struct{ dsn, ageKey, platformFile, want string }{
		"no dsn":             {"", good, "", "WARDYN_PG_DSN"},
		"no age key":         {unreachableDSN, "", "", "WARDYN_AGE_KEY (-age-key) is empty"},
		"the age key twice":  {unreachableDSN, good, keyFile(t, good), "same key as WARDYN_AGE_KEY"},
		"an unparseable key": {unreachableDSN, "AGE-SECRET-KEY-1NOPE", "", "parse the age identity"},
	} {
		f := rekeyFlags(c.dsn, "", c.ageKey)
		*f.platformKeyFile = c.platformFile
		if err := rewrapMode(f); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: rewrapMode = %v, want a refusal saying %q", label, err, c.want)
		}
	}
}
