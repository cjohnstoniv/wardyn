// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package kek

import (
	"context"
	"errors"
	"strings"
	"testing"

	"filippo.io/age"
)

// versionedKEK is a KEK whose wraps name a key version: the wrap is the version
// string itself, which is all Behind reads.
type versionedKEK struct {
	KEK
	err error
}

func (v versionedKEK) WrapVersion(wrapped []byte) (string, error) { return string(wrapped), v.err }

func (versionedKEK) LatestVersion(context.Context) (string, error) { return "v2", nil }

func TestBehind(t *testing.T) {
	local, err := NewLocalPurpose(mustIdentity(t), PurposeCred)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("unreadable wrap")

	for _, tc := range []struct {
		name    string
		k       KEK
		wrapped string
		latest  string
		want    bool
		wantErr error
	}{
		{"an unversioned KEK is never behind", local, "v1", "v2", false, nil},
		{"no latest version is never behind", versionedKEK{}, "v1", "", false, nil},
		{"an older version is behind", versionedKEK{}, "v1", "v2", true, nil},
		{"the latest version is not", versionedKEK{}, "v2", "v2", false, nil},
		{"an unreadable wrap is an error, not a verdict", versionedKEK{err: boom}, "v1", "v2", false, boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Behind(tc.k, []byte(tc.wrapped), tc.latest)
			if got != tc.want || !errors.Is(err, tc.wantErr) {
				t.Fatalf("Behind = %v, %v; want %v, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestSealAndOpenRefuseAKeyOfTheWrongSize(t *testing.T) {
	short := make([]byte, DEKSize-1)
	if _, err := Seal(short, []byte("v"), nil); err == nil || !strings.Contains(err.Error(), "want 32") {
		t.Errorf("Seal with a short key = %v, want a size refusal", err)
	}
	if _, err := Open(short, make([]byte, 64), nil); err == nil || !strings.Contains(err.Error(), "want 32") {
		t.Errorf("Open with a short key = %v, want a size refusal", err)
	}
}

func TestOpenRefusesWhatCannotBeASealedBlobWithoutLeakingInput(t *testing.T) {
	key := make([]byte, DEKSize)
	if _, err := Open(key, []byte("tiny"), nil); err == nil || !strings.Contains(err.Error(), "shorter than a nonce and a tag") {
		t.Errorf("Open of a truncated blob = %v", err)
	}
	sealed, err := Seal(key, []byte("the value"), []byte("aad-1"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(key, sealed, []byte("aad-2"))
	if err == nil || err.Error() != "authentication failed" {
		t.Errorf("Open under another AAD = %v, want the one opaque authentication failure", err)
	}
}

func TestLocalKEKRefusesWhatItWouldSealToTheWrongRow(t *testing.T) {
	l, err := NewLocalPurpose(mustIdentity(t), PurposeCred)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := l.Wrap(ctx, make([]byte, DEKSize), map[string]string{BindOwner: "alice"}); err == nil {
		t.Error("Wrap accepted a bind with no row name")
	}
	if _, err := l.Unwrap(ctx, make([]byte, 64), map[string]string{BindName: "x"}); err == nil {
		t.Error("Unwrap accepted a bind with no owner")
	}
	if _, err := l.Wrap(ctx, make([]byte, DEKSize-1), Bind("alice", "n")); err == nil || !strings.Contains(err.Error(), "data key is 31 bytes") {
		t.Errorf("Wrap of a short data key = %v", err)
	}
	if _, err := l.Unwrap(ctx, make([]byte, 64), Bind("alice", "n")); err == nil || !strings.Contains(err.Error(), "unwrap") {
		t.Errorf("Unwrap of a forged wrap = %v", err)
	}
}

func TestIsServiceID(t *testing.T) {
	for id, want := range map[string]bool{
		TransitIDPrefix + "wardyn":    true,
		AzureKeyIDPrefix + "k/1":      true,
		"local/cred:0123456789abcdef": false,
		"":                            false,
	} {
		if got := IsServiceID(id); got != want {
			t.Errorf("IsServiceID(%q) = %v, want %v", id, got, want)
		}
	}
}

func mustIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
