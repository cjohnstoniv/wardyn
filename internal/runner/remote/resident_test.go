// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"crypto/sha256"
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The org keeps digests of delivered values, never the values.
func TestLeafHashesHoldDigestsOnly(t *testing.T) {
	h := leafHashes{}
	h.add([]byte("plaintext-secret"))
	typ := reflect.TypeOf(h)
	if typ.Key() != reflect.TypeOf([sha256.Size]byte{}) || typ.Elem().Size() != 0 {
		t.Fatalf("leafHashes is %v, want a set of SHA-256 digests", typ)
	}
	if !h.has([]byte("plaintext-secret")) || !h.has([]byte(" plaintext-secret\n")) || h.has([]byte("plaintext")) || h.has(nil) {
		t.Fatal("has is not an exact-leaf match")
	}
}

func TestWalkLeavesReachesBytesAndMapValuesAndSkipsUnmarshalled(t *testing.T) {
	hit := func(spec runner.SandboxSpec, want string) bool {
		return walkLeaves(reflect.ValueOf(spec), func(b []byte) bool { return string(b) == want })
	}
	spec := runner.SandboxSpec{
		ManagedFiles: []runner.ManagedFile{{Content: []byte("in-bytes")}},
		SecretEnv:    map[string]string{"K": "in-map"},
		ProxyConfig:  runner.ProxyConfig{GitGrants: nil, MITMHosts: []string{"in-slice"}},
	}
	for _, want := range []string{"in-bytes", "in-map", "K", "in-slice"} {
		if !hit(spec, want) {
			t.Errorf("leaf %q not reached", want)
		}
	}
	if hit(spec, "absent") {
		t.Error("a leaf that is not there matched")
	}
	spec.OnWaiting = func(string) {} // json:"-": a callback is no leaf and must not panic the walk
	_ = hit(spec, "x")
}
